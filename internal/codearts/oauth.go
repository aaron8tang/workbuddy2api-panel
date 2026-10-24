// oauth.go CodeArts 登录与续期：portal 授权页（PKCE method 写 "SHA-256"）→
// 授权码换 STS 三元组 → 身份从 refresh token JWT 解出。refresh token 一次性，
// 刷新必须由调用方单飞并写回。
package codearts

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/hwauth"
)

// ErrAuthPending 登录未完成（轮询侧继续等）。
var ErrAuthPending = errors.New("authorization pending")

// httpx 渠道共享 HTTP 客户端。
var httpx = &http.Client{Timeout: 30 * time.Second}

// NewLoginContext 生成一轮登录的长期材料（PKCE + DPoP）。
// 两样都必须与最终落盘的凭据一起保存：授权码换出的 refresh_token 与签发时那把
// DPoP 公钥绑定，续期又要出示当初的 PKCE verifier。
func NewLoginContext() (*OAuthContext, error) {
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(seed)
	challenge := base64.RawURLEncoding.EncodeToString(sha256Sum([]byte(verifier)))
	pair, err := hwauth.GenerateKeyPair()
	if err != nil {
		return nil, err
	}
	return &OAuthContext{PkcePair: PkcePair{
		CodeVerifier:        verifier,
		CodeChallenge:       challenge,
		CodeChallengeMethod: PKCEMethod,
	}, DpopKeyPair: pair}, nil
}

func sha256Sum(b []byte) []byte { h := sha256.Sum256(b); return h[:] }

// encodeComponentJS 授权页参数用 JS encodeURIComponent 口径（与签名同一份）。
func encodeComponentJS(s string) string {
	b := []byte(s)
	const hexdigits = "0123456789ABCDEF"
	var sb strings.Builder
	for _, c := range b {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '!' || c == '~' || c == '*' ||
			c == '\'' || c == '(' || c == ')' {
			sb.WriteByte(c)
		} else {
			sb.WriteByte('%')
			sb.WriteByte(hexdigits[c>>4])
			sb.WriteByte(hexdigits[c&0x0f])
		}
	}
	return sb.String()
}

// AuthorizeURL 拼授权页地址。port 从 callback_url 读出（portal 只认 port，
// 路径它自己拼），redirect_uri 必须与换码时逐字相同（差一个斜杠就拒）。
func AuthorizeURL(ctx *OAuthContext, callbackURL, ticketID string) string {
	port := urlPort(callbackURL)
	pairs := [][2]string{
		{"theme", "2"},
		{"locale", "zh-cn"},
		{"uri_scheme", ClientID},
		{"client_id", ClientID},
		{"port", port},
		{"code_challenge", ctx.PkcePair.CodeChallenge},
		{"code_challenge_method", ctx.PkcePair.CodeChallengeMethod},
		{"ticket_id", ticketID},
		{"auth_callback_url", callbackURL},
		{"plugin-name", PluginName},
		{"plugin-version", PluginVersion},
	}
	qs := make([]string, 0, len(pairs))
	for _, kv := range pairs {
		qs = append(qs, kv[0]+"="+encodeComponentJS(kv[1]))
	}
	return WebLoginBase + "/portal/authorize?" + strings.Join(qs, "&")
}

func urlPort(callbackURL string) string {
	rest, ok := strings.CutPrefix(callbackURL, "http://")
	if !ok {
		rest = strings.TrimPrefix(callbackURL, "https://")
	}
	authority := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority = rest[:i]
	}
	if _, p, ok := strings.Cut(authority, ":"); ok {
		return p
	}
	return ""
}

// exchangeResponse 令牌端点响应（credentials 是嵌套段，别拉平了读）。
type exchangeResponse struct {
	Credentials struct {
		AccessKeyID  string `json:"access_key_id"`
		SecretAccess string `json:"secret_access_key"`
		SecurityTok  string `json:"security_token"`
		Expiration   string `json:"expiration"`
	} `json:"credentials"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`

	// 错误侧
	ErrorDescription string `json:"error_description"`
	Error            string `json:"error"`
	Message          string `json:"message"`
}

// tokenRequest 打一次令牌端点（两个 grant 共用；表单 + DPoP proof）。
func tokenRequest(form url.Values, ctx *OAuthContext) (*exchangeResponse, error) {
	if ctx == nil || strings.TrimSpace(ctx.PkcePair.CodeVerifier) == "" {
		return nil, errors.New("CodeArts 凭据缺少 PKCE verifier，无法续期，请重新登录")
	}
	key, err := hwauth.LoadKeyPair(ctx.DpopKeyPair)
	if err != nil {
		return nil, fmt.Errorf("DPoP 私钥不可用: %w", err)
	}
	proof, err := hwauth.DPoPProof(key, "POST", TokenURL, time.Now())
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("DPoP", proof)
	resp, err := httpx.Do(req)
	if err != nil {
		return nil, fmt.Errorf("CodeArts 令牌请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out exchangeResponse
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := out.ErrorDescription
		if detail == "" {
			detail = out.Error
		}
		if detail == "" {
			detail = out.Message
		}
		if detail == "" {
			detail = truncate(string(raw), 300)
		}
		status := resp.StatusCode
		if status >= 500 {
			status = http.StatusBadGateway
		}
		return nil, fmt.Errorf("CodeArts 令牌请求被拒（HTTP %d）: %s", resp.StatusCode, detail)
	}
	if out.Credentials.AccessKeyID == "" || out.Credentials.SecretAccess == "" || out.Credentials.SecurityTok == "" {
		return nil, errors.New("CodeArts 令牌响应缺少 access_key_id / secret_access_key / security_token")
	}
	if out.RefreshToken == "" {
		return nil, errors.New("CodeArts 令牌响应没有 refresh token，该凭据无法续期，拒绝收下（请重新登录）")
	}
	return &out, nil
}

// ExchangeCode 授权码 → 凭据。redirectURI 必须与交给 portal 的逐字相同。
func ExchangeCode(code, redirectURI string, ctx *OAuthContext) (*Credential, error) {
	form := url.Values{}
	form.Set("client_id", ClientID)
	form.Set("code", strings.TrimSpace(code))
	form.Set("code_verifier", strings.TrimSpace(ctx.PkcePair.CodeVerifier))
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", redirectURI)
	resp, err := tokenRequest(form, ctx)
	if err != nil {
		return nil, err
	}
	c := &Credential{
		AccessKeyID:  resp.Credentials.AccessKeyID,
		SecretAccess: resp.Credentials.SecretAccess,
		SecurityTok:  resp.Credentials.SecurityTok,
		ExpiresAt:    resp.Credentials.Expiration,
		RefreshToken: resp.RefreshToken,
		LoginType:    "WEB",
		OAuthContext: ctx,
	}
	// 身份优先从 refresh token JWT 载荷解（省一次 caller-identity 往返）。
	if account, principal, name := identityFromRefreshToken(c.RefreshToken); name != "" || account != "" {
		c.DomainID, c.UserID, c.UserName = account, principal, name
	}
	return c, nil
}

// RefreshCredential refresh_token grant。上游可能轮换 refresh token（响应给了
// 就用新的）；身份从旧凭据继承（续期响应不带身份）。
func RefreshCredential(existing *Credential) (*Credential, error) {
	ctx := existing.OAuthContext
	if ctx == nil || strings.TrimSpace(existing.RefreshToken) == "" {
		return nil, errors.New("CodeArts 凭据缺少 refresh token 或 OAuth 上下文，无法续期，请重新登录")
	}
	form := url.Values{}
	form.Set("client_id", ClientID)
	form.Set("code_verifier", strings.TrimSpace(ctx.PkcePair.CodeVerifier))
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", strings.TrimSpace(existing.RefreshToken))
	resp, err := tokenRequest(form, ctx)
	if err != nil {
		return nil, err
	}
	fresh := existing.clone()
	fresh.AccessKeyID = resp.Credentials.AccessKeyID
	fresh.SecretAccess = resp.Credentials.SecretAccess
	fresh.SecurityTok = resp.Credentials.SecurityTok
	fresh.ExpiresAt = resp.Credentials.Expiration
	fresh.RefreshToken = resp.RefreshToken
	if fresh.LoginType == "" {
		fresh.LoginType = "WEB"
	}
	return fresh, nil
}

// identityFromRefreshToken refresh token JWT payload → user_profile（再一层
// base64url JSON）→ account_id/principal_id/principal_urn；URN 末段是名字。
func identityFromRefreshToken(token string) (account, principal, name string) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return
	}
	claims, err := decodeB64JSON(parts[1])
	if err != nil {
		return
	}
	profileRaw, _ := claims["user_profile"].(string)
	profile, err := decodeB64JSON(profileRaw)
	if err != nil {
		return
	}
	account = strOf(profile["account_id"])
	principal = strOf(profile["principal_id"])
	urn := strOf(profile["principal_urn"])
	if account == "" || principal == "" || urn == "" {
		return "", "", ""
	}
	name = urn
	if i := strings.LastIndexAny(urn, ":/"); i >= 0 {
		name = urn[i+1:]
	}
	return
}

func decodeB64JSON(segment string) (map[string]any, error) {
	segment = strings.TrimRight(strings.TrimSpace(segment), "=")
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func strOf(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---------------------------------------------------------------------------
// 登录事务（面板 Web 登录：生成授权 URL + 轮询回调查询串换码）
// ---------------------------------------------------------------------------

// LoginTransaction 一轮进行中的登录。
type LoginTransaction struct {
	TicketID    string
	Ctx         *OAuthContext
	CallbackURL string
	CreatedAt   time.Time
}

// LoginTTL 登录事务有效期。
const LoginTTL = 10 * time.Minute

// StartLogin 发起一轮登录：生成上下文与 32 hex ticket，返回授权 URL。
// 回调 URL 指向本机 callbackPort（容器内依赖端口映射；对齐 trae 全网卡绑定的考量，
// 监听侧由 Gateway.StartCallbackServer 处理）。
func StartLogin(callbackPort string) (*LoginTransaction, error) {
	ctx, err := NewLoginContext()
	if err != nil {
		return nil, err
	}
	seed := make([]byte, 16)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	ticket := hex.EncodeToString(seed)
	callback := "http://127.0.0.1:" + callbackPort + CallbackPath
	return &LoginTransaction{
		TicketID:    ticket,
		Ctx:         ctx,
		CallbackURL: callback,
		CreatedAt:   time.Now(),
	}, nil
}

// ParseCallback 解析回调查询串：第一趟带 secret+redirect（跳转用），第二趟带
// code（可直接换码，也可由调用方按 ticket/候选换码）。
func ParseCallback(query url.Values) (code, secret, redirect, ticket string) {
	return strings.TrimSpace(query.Get("code")),
		strings.TrimSpace(query.Get("secret")),
		strings.TrimSpace(query.Get("redirect")),
		strings.TrimSpace(query.Get("ticket_id"))
}

// loginRegistry 进程内待办表（cap 8、超时挤出最旧，对齐 agent2api oauth.rs）。
type loginRegistry struct {
	mu   sync.Mutex
	ttls map[string]*LoginTransaction
}

// Registry 渠道级登录注册表（Gateway 持有）。
type Registry struct{ r loginRegistry }

// NewRegistry 建空注册表。
func NewRegistry() *Registry { return &Registry{r: loginRegistry{ttls: map[string]*LoginTransaction{}}} }

// Add 登记一轮；满 8 个挤掉最旧。
func (rg *Registry) Add(tx *LoginTransaction) {
	rg.r.mu.Lock()
	defer rg.r.mu.Unlock()
	now := time.Now()
	for id, t := range rg.r.ttls {
		if now.Sub(t.CreatedAt) > LoginTTL {
			delete(rg.r.ttls, id)
		}
	}
	if len(rg.r.ttls) >= 8 {
		var oldestID string
		var oldest time.Time
		for id, t := range rg.r.ttls {
			if oldestID == "" || t.CreatedAt.Before(oldest) {
				oldestID, oldest = id, t.CreatedAt
			}
		}
		if oldestID != "" {
			delete(rg.r.ttls, oldestID)
		}
	}
	rg.r.ttls[tx.TicketID] = tx
}

// Peek 只读一份（换码失败这一轮必须留在表里——/oauth/callback 免鉴权，
// 取走式写法会把「取消别人的登录」做成免费服务）。
func (rg *Registry) Peek(ticketID string) *LoginTransaction {
	rg.r.mu.Lock()
	defer rg.r.mu.Unlock()
	tx := rg.r.ttls[ticketID]
	if tx != nil && time.Since(tx.CreatedAt) > LoginTTL {
		return nil
	}
	return tx
}

// Take 取走一轮（成功才调）。
func (rg *Registry) Take(ticketID string) *LoginTransaction {
	rg.r.mu.Lock()
	defer rg.r.mu.Unlock()
	tx := rg.r.ttls[ticketID]
	delete(rg.r.ttls, ticketID)
	return tx
}

// Drop 放弃一轮。
func (rg *Registry) Drop(ticketID string) {
	rg.r.mu.Lock()
	defer rg.r.mu.Unlock()
	delete(rg.r.ttls, ticketID)
}

// Candidates 全部有效轮次（新→旧）。portal 回调实测可能只带 code 不带任何
// 配对信息，所以只能逐个候选试 verifier；用错 verifier 会被 STS 判 STS5.1805
// 而不消耗授权码，逐个试是安全的。
func (rg *Registry) Candidates() []*LoginTransaction {
	rg.r.mu.Lock()
	defer rg.r.mu.Unlock()
	now := time.Now()
	out := make([]*LoginTransaction, 0, len(rg.r.ttls))
	for id, t := range rg.r.ttls {
		if now.Sub(t.CreatedAt) > LoginTTL {
			delete(rg.r.ttls, id)
			continue
		}
		out = append(out, t)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CreatedAt.After(out[j-1].CreatedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// logf 渠道日志。
func logf(format string, args ...any) { log.Printf("[codearts] "+format, args...) }
