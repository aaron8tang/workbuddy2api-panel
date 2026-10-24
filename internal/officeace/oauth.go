// oauth.go OfficeAce 自助 OAuth 九步：云端 state → PKCE+DPoP → 授权页（回调指向
// 云端）→ 轮询取码 → STS 换临时 AK/SK → 权限校验（拿 model_info）→ 未开通自动开通
// → 解网关三件套。refresh token 一次性轮换。
package officeace

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/hwauth"
)

var httpx = &http.Client{Timeout: 30 * time.Second}

// ErrAuthPending 还没登完（202）。
var ErrAuthPending = errors.New("authorization pending")

// urlEscape encodeURIComponent 口径（PKCE challenge 与 URL 参数共用）。
func urlEscape(s string) string {
	const hexdigits = "0123456789ABCDEF"
	var sb strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			sb.WriteByte(c)
		} else {
			sb.WriteByte('%')
			sb.WriteByte(hexdigits[c>>4])
			sb.WriteByte(hexdigits[c&0x0f])
		}
	}
	return sb.String()
}

// LoginFlow 一轮进行中的登录。
type LoginFlow struct {
	State       string
	AuthURL     string
	Verifier    string
	DpopKeyPair hwauth.KeyPair
}

// StartLogin 九步之 ①②③：云端取 state → PKCE+DPoP → 拼授权 URL。
func StartLogin() (*LoginFlow, error) {
	state, err := fetchState()
	if err != nil {
		return nil, err
	}
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
	flow := &LoginFlow{State: state, Verifier: verifier, DpopKeyPair: pair}
	flow.AuthURL = buildAuthorizeURL(state, challenge)
	return flow, nil
}

func sha256Sum(b []byte) []byte { h := sha256.Sum256(b); return h[:] }

// fetchState ① POST /v1/claw/auth/state（无鉴权）。
func fetchState() (string, error) {
	req, err := http.NewRequest(http.MethodPost, CloudBase+"/v1/claw/auth/state", strings.NewReader("{}"))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := httpx.Do(req)
	if err != nil {
		return "", fmt.Errorf("向云端申请 state 失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	state, _ := m["state"].(string)
	state = strings.TrimSpace(state)
	if state == "" {
		return "", fmt.Errorf("云端没给出 state（HTTP %d）", resp.StatusCode)
	}
	return state, nil
}

// buildAuthorizeURL ③ 官方写法：外层 login.html?service=<内层 authorize>。
func buildAuthorizeURL(state, challenge string) string {
	inner := AuthBase + "/authui/v1/oauth2/authorize?client_id=" + ClientID +
		"&code_challenge=" + urlEscape(challenge) +
		"&code_challenge_method=SHA-256" +
		"&state=" + urlEscape(state) +
		"&scope=openid&redirect_uri=" + urlEscape(RedirectURI) +
		"&response_type=code"
	return AuthBase + "/authui/login.html?service=" + urlEscape(inner)
}

// PollCode ④ 轮询取码。202 = 未登完继续等；404 = state 作废立即失败。
func PollCode(state string) (string, error) {
	req, err := http.NewRequest(http.MethodGet,
		CloudBase+"/v1/claw/auth/code?state="+urlEscape(state), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := httpx.Do(req)
	if err != nil {
		return "", fmt.Errorf("轮询授权码失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch resp.StatusCode {
	case http.StatusAccepted:
		return "", ErrAuthPending
	case http.StatusNotFound:
		return "", errors.New("授权码已失效或 state 不认识，请重新发起登录")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("取授权码返回 HTTP %d", resp.StatusCode)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if e, _ := m["error"].(string); e != "" {
		msg, _ := m["error_msg"].(string)
		if msg == "" {
			msg = e
		}
		return "", fmt.Errorf("云端拒绝: %s", msg)
	}
	code, _ := m["code"].(string)
	if code == "" {
		code, _ = m["authorization_code"].(string)
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return "", errors.New("云端说好了，但没给授权码")
	}
	return code, nil
}

// gatewayInfo model_info → 网关三件套。
type gatewayInfo struct {
	BaseURL    string
	AppKey     string
	AppSecret  string
}

// ExchangeCode ⑤⑥⑦：授权码换临时凭据 → project_id 补查 → 权限校验（未开通自动
// 开通）→ 网关三件套。
func ExchangeCode(flow *LoginFlow, code string) (*Credential, error) {
	// ⑤ 换临时 AK/SK（表单 + DPoP）。
	resp, err := tokenPost(url.Values{
		"client_id":     {ClientID},
		"code":          {strings.TrimSpace(code)},
		"code_verifier": {flow.Verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {RedirectURI},
	}, flow.DpopKeyPair)
	if err != nil {
		return nil, err
	}
	cred := &Credential{
		AccessKeyID:  resp.cred.AccessKeyID,
		SecretAccess: resp.cred.SecretKey,
		SecurityTok:  resp.cred.SecurityToken,
		ProjectID:    resp.cred.ProjectID,
		ExpiresAtMs:  resp.expiresAt,
		RefreshToken: resp.refreshToken,
		DpopKeyPair:  flow.DpopKeyPair,
	}
	// ⑥a project_id 补查（令牌响应不带；查不到不阻断）。
	if cred.ProjectID == "" {
		cred.ProjectID = resolveProjectID(cred)
	}
	// ⑥ 权限校验（拿 model_info；可能为空 = 账号尚未开通）。
	modelInfo, accountID, principalID, err := fetchPermission(cred)
	if err != nil {
		return nil, err
	}
	// ⑥b 未开通自动开通。
	if modelInfo == nil {
		modelInfo, err = subscribeModelInfo(cred)
		if err != nil {
			return nil, err
		}
	}
	info, err := gatewayOf(modelInfo)
	if err != nil {
		return nil, err
	}
	cred.BaseURL = info.BaseURL
	cred.ModelAppKey = info.AppKey
	cred.ModelAppSecret = info.AppSecret
	// 显示名：refresh_token JWT 的 user_profile.account_name（实测名字就在这一枚）。
	if name, principal := jwtIdentity(resp.idToken, resp.refreshToken); name != "" {
		cred.Nickname = name
	} else if principal != "" {
		cred.Nickname = principal
	}
	if cred.Nickname == "" {
		cred.Nickname = accountID
	}
	if principalID != "" && cred.Nickname == accountID {
		cred.Nickname = principalID
	}
	return cred, nil
}

// tokenResult 令牌端点结果。
type tokenResult struct {
	cred struct {
		AccessKeyID   string `json:"access_key_id"`
		SecretKey     string `json:"secret_access_key"`
		SecurityToken string `json:"security_token"`
		ProjectID     string `json:"project_id"`
		Expiration    string `json:"expiration"`
	}
	refreshToken string
	idToken      string
	expiresAt    int64
}

// tokenPost 打令牌端点（两 grant 共用；表单 + DPoP proof，私钥必须是当初登录那把）。
func tokenPost(form url.Values, pair hwauth.KeyPair) (*tokenResult, error) {
	key, err := hwauth.LoadKeyPair(pair)
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
		return nil, fmt.Errorf("令牌请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var payload struct {
		Credentials struct {
			AccessKeyID  string `json:"access_key_id"`
			SecretAccess string `json:"secret_access_key"`
			SecurityTok  string `json:"security_token"`
			ProjectID    string `json:"project_id"`
			Expiration   string `json:"expiration"`
		} `json:"credentials"`
		RefreshToken     string `json:"refresh_token"`
		IDToken          string `json:"id_token"`
		ErrorDescription string `json:"error_description"`
		ErrorMsg         string `json:"error_msg"`
		Error            string `json:"error"`
	}
	_ = json.Unmarshal(raw, &payload)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 错误体可能回显 refresh_token——只取 code/message 字段。
		detail := payload.ErrorDescription
		if detail == "" {
			detail = payload.ErrorMsg
		}
		if detail == "" {
			detail = payload.Error
		}
		return nil, fmt.Errorf("令牌端点被拒（HTTP %d）: %s", resp.StatusCode, detail)
	}
	if payload.Credentials.AccessKeyID == "" || payload.Credentials.SecretAccess == "" {
		return nil, errors.New("令牌端点没返回临时凭据")
	}
	out := &tokenResult{
		refreshToken: payload.RefreshToken,
		idToken:      payload.IDToken,
		expiresAt:    time.Now().Add(2 * time.Hour).UnixMilli(), // 实测约 2 小时
	}
	out.cred.AccessKeyID = payload.Credentials.AccessKeyID
	out.cred.SecretKey = payload.Credentials.SecretAccess
	out.cred.SecurityToken = payload.Credentials.SecurityTok
	out.cred.ProjectID = payload.Credentials.ProjectID
	if ms, ok := parseRFC3339Ms(payload.Credentials.Expiration); ok && ms > 0 {
		out.expiresAt = ms
	}
	return out, nil
}

// RefreshLogin 用一次性 refresh_token 换新的临时 AK/SK（同一把 DPoP 私钥签 proof）。
// 返回 (新控制面凭据, 到期毫秒, 显示名, 轮换后的 refresh_token)。
func RefreshLogin(pair hwauth.KeyPair, refreshToken string) (ak, sk, st, pid string, expiresAt int64, userName, nextRefresh string, err error) {
	if strings.TrimSpace(refreshToken) == "" {
		return "", "", "", "", 0, "", "", errors.New("OfficeAce 账号没有 refresh token，只能重新登录授权")
	}
	resp, err := tokenPost(url.Values{
		"client_id":     {ClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {strings.TrimSpace(refreshToken)},
	}, pair)
	if err != nil {
		return "", "", "", "", 0, "", "", err
	}
	name, _ := jwtIdentity(resp.idToken, resp.refreshToken)
	next := resp.refreshToken
	if next == "" {
		next = refreshToken // 没给就沿用旧的（一次性的，沿用也只是再赌一次）
	}
	return resp.cred.AccessKeyID, resp.cred.SecretKey, resp.cred.SecurityToken,
		resp.cred.ProjectID, resp.expiresAt, name, next, nil
}

// resolveProjectId IAM keystone 查 project（按 region 名匹配启用中的项目）。
func resolveProjectID(cred *Credential) string {
	signed, err := hwauth.SignOfficeAceSDK(http.MethodGet,
		"https://iam.myhuaweicloud.com/v3/projects", nil, nil, cred.Signing())
	if err != nil {
		return ""
	}
	req, err := http.NewRequest(http.MethodGet, "https://iam.myhuaweicloud.com/v3/projects", nil)
	if err != nil {
		return ""
	}
	for _, kv := range signed.Headers {
		req.Header.Set(kv[0], kv[1])
	}
	resp, err := httpx.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var payload struct {
		Projects []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		} `json:"projects"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	for _, p := range payload.Projects {
		if p.Name == Region && p.Enabled && p.ID != "" {
			return p.ID
		}
	}
	return ""
}

// fetchPermission ⑥ 权限校验（SDK 签名 + x-subscription-type: v2）。
// model_info 三种位置都兜：顶层 → subscription.model_info → modelInfo；非对象当没有。
func fetchPermission(cred *Credential) (modelInfo map[string]any, accountID, principalID string, err error) {
	rawURL := CloudBase + "/v1/claw/client-permission-validate"
	headers := [][2]string{{"x-subscription-type", "v2"}}
	signed, err := hwauth.SignOfficeAceSDK(http.MethodGet, rawURL, headers, nil, cred.Signing())
	if err != nil {
		return nil, "", "", err
	}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", "", err
	}
	for _, kv := range signed.Headers {
		req.Header.Set(kv[0], kv[1])
	}
	resp, err := httpx.Do(req)
	if err != nil {
		return nil, "", "", fmt.Errorf("权限校验失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var payload map[string]any
	_ = json.Unmarshal(raw, &payload)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := stringOf(payload["error_msg"])
		if detail == "" {
			detail = stringOf(payload["error_code"])
		}
		return nil, "", "", fmt.Errorf("权限校验失败（HTTP %d）: %s", resp.StatusCode, detail)
	}
	accountID = stringOf(payload["account_id"])
	principalID = stringOf(payload["principal_id"])
	for _, key := range []string{"model_info", "modelInfo"} {
		if m, ok := payload[key].(map[string]any); ok {
			return m, accountID, principalID, nil
		}
	}
	if sub, ok := payload["subscription"].(map[string]any); ok {
		if m, ok := sub["model_info"].(map[string]any); ok {
			return m, accountID, principalID, nil
		}
	}
	return nil, accountID, principalID, nil
}

// subscribeModelInfo ⑥b 开通（POST client-subscription，body {}，成功返回顶层 model_info）。
func subscribeModelInfo(cred *Credential) (map[string]any, error) {
	rawURL := CloudBase + "/v1/claw/client-subscription"
	headers := [][2]string{
		{"Content-Type", "application/json;charset=utf8"},
		{"x-subscription-type", "v2"},
	}
	signed, err := hwauth.SignOfficeAceSDK(http.MethodPost, rawURL, headers, []byte("{}"), cred.Signing())
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, rawURL, strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	for _, kv := range signed.Headers {
		req.Header.Set(kv[0], kv[1])
	}
	resp, err := httpx.Do(req)
	if err != nil {
		return nil, fmt.Errorf("开通失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var payload map[string]any
	_ = json.Unmarshal(raw, &payload)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("开通失败（HTTP %d）: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	if m, ok := payload["model_info"].(map[string]any); ok {
		return m, nil
	}
	// 邀请码族错误码 → 人话。
	code := stringOf(payload["error_code"])
	switch code {
	case "AgentArts.11000008":
		return nil, errors.New("开通失败：邀请码无效")
	case "AgentArts.11000009":
		return nil, errors.New("开通失败：邀请码不存在或今日配额已用完")
	case "AgentArts.00000104", "common.01010003":
		return nil, errors.New("开通失败：当前账号权限不足")
	case "common.01010004":
		return nil, errors.New("开通失败：账号可能未实名认证或处于欠费状态")
	}
	return nil, fmt.Errorf("开通未返回 model_info: %s", truncate(string(raw), 300))
}

// gatewayOf model_info → 网关三件套。
func gatewayOf(modelInfo map[string]any) (gatewayInfo, error) {
	authInfo, _ := modelInfo["model_auth_info"].(map[string]any)
	key := strings.TrimSpace(stringOf(authInfo["model_app_key"]))
	secret := strings.TrimSpace(stringOf(authInfo["model_app_secret"]))
	if key == "" || secret == "" {
		return gatewayInfo{}, errors.New("云端返回了模型信息，但缺少模型网关凭据（model_app_key / model_app_secret）")
	}
	host := strings.TrimSpace(stringOf(modelInfo["model_api_url_base"]))
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host = strings.Trim(host, "/")
	info := gatewayInfo{AppKey: key, AppSecret: secret}
	if host == "" {
		return info, nil
	}
	if strings.HasSuffix(host, "/v2") {
		info.BaseURL = "https://" + host
	} else {
		info.BaseURL = "https://" + host + "/v2"
	}
	return info, nil
}

// jwtIdentity 从 id_token / refresh_token 两枚 JWT 解身份（顶层 preferred_username
// → name → user_profile（base64 JSON）.account_name）。全程 fail-open。
func jwtIdentity(idToken, refreshToken string) (userName, principalID string) {
	for _, tok := range []string{idToken, refreshToken} {
		claims, err := decodeJWTClaims(tok)
		if err != nil {
			continue
		}
		name := stringOf(claims["preferred_username"])
		if name == "" {
			name = stringOf(claims["name"])
		}
		principal := stringOf(claims["principal_id"])
		if profileRaw, ok := claims["user_profile"].(string); ok {
			profile, perr := decodeB64JSONFlexible(profileRaw)
			if perr == nil {
				if name == "" {
					name = stringOf(profile["account_name"])
				}
				if principal == "" {
					principal = stringOf(profile["principal_id"])
				}
			}
		}
		if name != "" || principal != "" {
			return name, principal
		}
	}
	return "", ""
}

func decodeJWTClaims(token string) (map[string]any, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return nil, errors.New("not a jwt")
	}
	return decodeB64JSONFlexible(parts[1])
}

// decodeB64JSONFlexible 无填充 base64，URL_SAFE 与 STANDARD 两种字母表都试。
func decodeB64JSONFlexible(s string) (map[string]any, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, "=")
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(s)
		if err != nil {
			return nil, err
		}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func stringOf(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// parseRFC3339Ms 只支持 YYYY-MM-DDTHH:MM:SS[.mmm]Z。
func parseRFC3339Ms(input string) (int64, bool) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(input))
	if err != nil {
		return 0, false
	}
	return t.UnixMilli(), true
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
