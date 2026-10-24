// Package qoder 实现 Qoder（qoder.sh 国际站）渠道：
// OAuth 设备流登录（PKCE）+ 每账号持久运行时字段 + COSY 签名对话
//（internal/cosy 共用底座，ProfileQoder 变体）。
// 移植自 thirdparty/qoder/API-Console-main/internal/qoder，只取 Qoder 相关协议，
// 底座算法收敛到 internal/cosy。
package qoder

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cosy"
)

// RefreshLead 刷新提前量（容忍时钟偏移，不立刻刷新短时令牌）。
const RefreshLead = time.Minute

// LoginTTL 设备授权事务有效期；loginInterval 轮询间隔。
const (
	LoginTTL      = 15 * time.Minute
	loginInterval = 2 * time.Second
)

// ErrAuthPending 浏览器侧尚未完成授权（poll 端点以 404 表示 pending）。
var ErrAuthPending = errors.New("qoder authorization is still pending")

// ErrReLoginRequired 持久凭证已失效（refresh 被拒）——只能重新浏览器授权。
var ErrReLoginRequired = errors.New("qoder refresh token is expired; a new browser login is required")

// Cred 一个账号的设备凭证 + 派生运行时字段。
// MachineID 是凭证授权时绑定的设备身份（上游拒绝其他身份的签名）。
type Cred struct {
	UID       string
	Name      string
	Email     string
	OrgID     string
	OrgTags   []string
	MachineID string // OAuth 授权时绑定的设备 ID
	DT        string // AccessToken（device token）
	DRT       string // RefreshToken（durable secret，轮换）
	DTExpiry  time.Time
	DRTExpiry time.Time

	// 派生运行时字段（等价 cosy.Session 的 CosyKey/Info），登录/刷新后重派生并落盘。
	RuntimeKey  string
	RuntimeInfo string

	mu sync.Mutex
}

// AccessValid 报告 access token 在 now 时刻是否可用。
func (c *Cred) AccessValid(now time.Time) bool {
	if strings.TrimSpace(c.DT) == "" {
		return false
	}
	if c.DTExpiry.IsZero() {
		return true // 无可解码过期时间的 token 直接试用，上游是权威
	}
	return c.DTExpiry.Sub(now) > RefreshLead
}

// HasCredential 是否带任何可用凭证。
func (c *Cred) HasCredential() bool {
	return strings.TrimSpace(c.DT) != "" || strings.TrimSpace(c.DRT) != ""
}

// identity 组装 COSY identity（Qoder 侧 user_type 故意为空——国际端不发账号类别）。
func (c *Cred) identity() cosy.Identity {
	return cosy.Identity{
		Name:               c.Name,
		Aid:                c.UID,
		UID:                c.UID,
		SecurityOAuthToken: c.DT,
		RefreshToken:       c.DRT,
	}
}

// EnsureRuntime 返回可用的运行时字段：已派生且匹配当前 token 对则复用，
// 否则重派生（token 轮换后 identity 内嵌的 dt/drt 变化，必须重派生）。
func (c *Cred) EnsureRuntime() (key, info string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.RuntimeKey != "" && c.RuntimeInfo != "" && c.DT != "" {
		return c.RuntimeKey, c.RuntimeInfo, nil
	}
	key, info, err = cosy.DeriveRuntime(c.identity())
	if err != nil {
		return "", "", err
	}
	c.RuntimeKey, c.RuntimeInfo = key, info
	return key, info, nil
}

// InvalidateRuntime token 轮换后清空运行时字段（下次 EnsureRuntime 重派生）。
func (c *Cred) InvalidateRuntime() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.RuntimeKey, c.RuntimeInfo = "", ""
}

// ---------------------------------------------------------------------------
// 落盘（qoder-<uid>.json）
// ---------------------------------------------------------------------------

// ParseCred 从原始 JSON 解析凭证（src 仅用于错误提示与文件名兜底）。
func ParseCred(raw []byte, src ...string) (*Cred, error) {
	where := ""
	if len(src) > 0 {
		where = src[0]
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	var c Cred
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken      string `json:"accessToken"`
				RefreshToken     string `json:"refreshToken"`
				ExpiresAt        string `json:"expiresAt"`
				RefreshExpiresAt string `json:"refreshExpiresAt"`
			} `json:"auth"`
			Account struct {
				UID   string `json:"uid"`
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"account"`
			Organization struct {
				ID   string   `json:"id"`
				Tags []string `json:"tags"`
			} `json:"organization"`
			MachineID string `json:"machine_id"`
			Runtime   struct {
				Key  string `json:"key"`
				Info string `json:"info"`
			} `json:"runtime"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, err
		}
		c = Cred{
			DT:          n.Auth.AccessToken,
			DRT:         n.Auth.RefreshToken,
			DTExpiry:    parseExpiry(n.Auth.ExpiresAt, 0, time.Now()),
			DRTExpiry:   parseExpiry(n.Auth.RefreshExpiresAt, 0, time.Now()),
			UID:         n.Account.UID,
			Name:        n.Account.Name,
			Email:       n.Account.Email,
			OrgID:       n.Organization.ID,
			OrgTags:     n.Organization.Tags,
			MachineID:   n.MachineID,
			RuntimeKey:  n.Runtime.Key,
			RuntimeInfo: n.Runtime.Info,
		}
	} else {
		// 扁平兼容：{"token"/"accessToken","refresh_token","uid"/"user_id","expires_at_unix"}
		var f struct {
			Token        string `json:"token"`
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refresh_token"`
			ExpiresAt    int64  `json:"expires_at_unix"`
			UID          string `json:"uid"`
			UserID       string `json:"user_id"`
			Name         string `json:"name"`
			MachineID    string `json:"machine_id"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, err
		}
		c.DT = f.Token
		if c.DT == "" {
			c.DT = f.AccessToken
		}
		c.DRT = f.RefreshToken
		if f.ExpiresAt > 0 {
			c.DTExpiry = unixSeconds(f.ExpiresAt)
		}
		c.UID = f.UID
		if c.UID == "" {
			c.UID = f.UserID
		}
		c.Name = f.Name
		c.MachineID = f.MachineID
	}
	if !c.HasCredential() {
		return nil, fmt.Errorf("missing dt/drt in %s", where)
	}
	if c.UID == "" {
		base := filepath.Base(where)
		if strings.HasPrefix(base, "qoder-") && strings.HasSuffix(base, ".json") {
			c.UID = strings.TrimSuffix(strings.TrimPrefix(base, "qoder-"), ".json")
		}
	}
	if c.UID == "" {
		return nil, fmt.Errorf("missing uid in %s", where)
	}
	return &c, nil
}

// LoadFile 从磁盘加载凭证。
func LoadFile(path string) (*Cred, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseCred(raw, path)
}

// LoadDir 扫描目录下 qoder-*.json，跳过解析失败的。
func LoadDir(dir string) ([]*Cred, error) {
	files, err := filepath.Glob(filepath.Join(dir, "qoder-*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Cred
	for _, f := range files {
		c, err := LoadFile(f)
		if err != nil {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// FilePath 凭证落盘路径。
func (c *Cred) FilePath(dir string) string {
	return filepath.Join(dir, "qoder-"+c.UID+".json")
}

// Save 凭证写盘（嵌套形态）。
func (c *Cred) Save(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tags := c.OrgTags
	if tags == nil {
		tags = []string{}
	}
	raw, err := json.MarshalIndent(map[string]any{
		"auth": map[string]any{
			"accessToken":      c.DT,
			"refreshToken":     c.DRT,
			"expiresAt":        c.DTExpiry.Format(time.RFC3339),
			"refreshExpiresAt": c.DRTExpiry.Format(time.RFC3339),
		},
		"account":      map[string]any{"uid": c.UID, "name": c.Name, "email": c.Email},
		"organization": map[string]any{"id": c.OrgID, "tags": tags},
		"machine_id":   c.MachineID,
		"runtime":      map[string]any{"key": c.RuntimeKey, "info": c.RuntimeInfo},
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.FilePath(dir), raw, 0o600)
}

// ---------------------------------------------------------------------------
// OAuth 设备流（PKCE）
// ---------------------------------------------------------------------------

// LoginTransaction 一次设备授权事务。Nonce/Verifier 是私有半边，不下发浏览器。
type LoginTransaction struct {
	VerifyURL string    `json:"verify_url"`
	Nonce     string    `json:"nonce"`
	Verifier  string    `json:"verifier"`
	MachineID string    `json:"machine_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// OAuthConfig 设备流参数（来自 Profile）。
type OAuthConfig struct {
	OAuthBase     string // https://qoder.com
	OpenAPIBase   string // https://openapi.qoder.sh
	ClientID      string
	ClientVersion string
}

// pkcePair 生成设备流 verifier 与 S256 challenge（43..128 字符非保留集）。
func pkcePair() (verifier, challenge string, err error) {
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	var lengthByte [1]byte
	if _, err := rand.Read(lengthByte[:]); err != nil {
		return "", "", err
	}
	length := 43 + int(lengthByte[0]%86)
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	encoded := make([]byte, length)
	for i, b := range raw {
		encoded[i] = charset[int(b)%len(charset)]
	}
	verifier = string(encoded)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// StartLogin 发起一次设备授权事务（返回浏览器授权 URL）。
func StartLogin(oc OAuthConfig) (*LoginTransaction, error) {
	verifier, challenge, err := pkcePair()
	if err != nil {
		return nil, err
	}
	nonce := cosy.UUID()
	machineID := cosy.UUID()
	query := url.Values{}
	query.Set("challenge", challenge)
	query.Set("challenge_method", "S256")
	query.Set("nonce", nonce)
	query.Set("machine_id", machineID)
	query.Set("client_id", oc.ClientID)
	if oc.ClientVersion != "" {
		query.Set("client_version", oc.ClientVersion)
	}
	return &LoginTransaction{
		VerifyURL: strings.TrimRight(oc.OAuthBase, "/") + "/device/selectAccounts?" + query.Encode(),
		Nonce:     nonce,
		Verifier:  verifier,
		MachineID: machineID,
		ExpiresAt: time.Now().Add(LoginTTL),
	}, nil
}

// PollLogin 轮询设备授权结果。404 = ErrAuthPending（浏览器侧未完成）。
// 成功返回凭证（含上游观察到的 uid/name）。
func PollLogin(oc OAuthConfig, tx *LoginTransaction, cl *httpClient) (*Cred, error) {
	if tx == nil {
		return nil, errors.New("qoder login transaction is missing")
	}
	query := url.Values{}
	query.Set("nonce", tx.Nonce)
	query.Set("verifier", tx.Verifier)
	query.Set("challenge_method", "S256")
	endpoint := oc.OpenAPIBase + "/api/v1/deviceToken/poll?" + query.Encode()
	status, raw, err := cl.do(http.MethodGet, endpoint, nil, nil)
	if err != nil {
		return nil, err
	}
	switch {
	case status == 404:
		return nil, ErrAuthPending
	case status != 200:
		return nil, fmt.Errorf("qoder authorization was rejected: poll http %d: %.200s", status, raw)
	}
	var token struct {
		Token            string `json:"token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresAt        string `json:"expires_at"`
		ExpiresIn        int64  `json:"expires_in"`
		RefreshExpiresAt string `json:"refresh_token_expires_at"`
		RefreshExpiresIn int64  `json:"refresh_token_expires_in"`
		UserID           string `json:"user_id"`
		UserName         string `json:"user_name"`
	}
	if err := json.Unmarshal(raw, &token); err != nil {
		return nil, fmt.Errorf("qoder authorization undecodable: %w", err)
	}
	if strings.TrimSpace(token.Token) == "" {
		return nil, errors.New("qoder authorization carried no token")
	}
	now := time.Now()
	return &Cred{
		DT:        strings.TrimSpace(token.Token),
		DRT:       strings.TrimSpace(token.RefreshToken),
		DTExpiry:  parseExpiry(token.ExpiresAt, token.ExpiresIn, now),
		DRTExpiry: parseExpiry(token.RefreshExpiresAt, token.RefreshExpiresIn, now),
		UID:       strings.TrimSpace(token.UserID),
		Name:      strings.TrimSpace(token.UserName),
		MachineID: tx.MachineID,
	}, nil
}

// Refresh 用 DRT 换新 token 对（上游轮换 DRT，返回对必须持久化）。
// 401/403 → ErrReLoginRequired。
func (c *Cred) Refresh(oc OAuthConfig, cl *httpClient) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	drt := strings.TrimSpace(c.DRT)
	if drt == "" {
		return errors.New("qoder account is missing an OAuth credential; sign in again")
	}
	body, _ := json.Marshal(map[string]string{"refresh_token": drt})
	endpoint := oc.OpenAPIBase + "/api/v1/deviceToken/refresh"
	headers := map[string]string{"Content-Type": "application/json", "Accept": "application/json"}
	status, raw, err := cl.do(http.MethodPost, endpoint, body, headers)
	if err != nil {
		return err
	}
	if status != 200 {
		if status == 401 || status == 403 {
			return fmt.Errorf("%w: refresh http %d: %.200s", ErrReLoginRequired, status, raw)
		}
		return fmt.Errorf("qoder refresh http %d: %.200s", status, raw)
	}
	var payload struct {
		DeviceToken      string `json:"device_token"`
		Token            string `json:"token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresAt        string `json:"expires_at"`
		ExpiresIn        int64  `json:"expires_in"`
		RefreshExpiresAt string `json:"refresh_token_expires_at"`
		RefreshExpiresIn int64  `json:"refresh_token_expires_in"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("decode refresh response: %w", err)
	}
	access := payload.DeviceToken
	if access == "" {
		access = payload.Token
	}
	if strings.TrimSpace(access) == "" {
		return fmt.Errorf("%w: refresh response carried no token", ErrReLoginRequired)
	}
	now := time.Now()
	c.DT = strings.TrimSpace(access)
	if rt := strings.TrimSpace(payload.RefreshToken); rt != "" {
		c.DRT = rt
	}
	c.DTExpiry = parseExpiry(payload.ExpiresAt, payload.ExpiresIn, now)
	c.DRTExpiry = parseExpiry(payload.RefreshExpiresAt, payload.RefreshExpiresIn, now)
	c.RuntimeKey, c.RuntimeInfo = "", "" // 重派生
	return nil
}

// EnsureDT 保证 DT 可用（余量 > RefreshLead 直接用；否则 refresh）。
func (c *Cred) EnsureDT(oc OAuthConfig, cl *httpClient) error {
	c.mu.Lock()
	if c.AccessValid(time.Now()) {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()
	if err := c.Refresh(oc, cl); err != nil {
		return err
	}
	return nil
}

// Profile /api/v1/userinfo 返回的账号身份（可选元数据）。
type Profile struct {
	UID     string   `json:"uid"`
	Name    string   `json:"name"`
	Email   string   `json:"email"`
	OrgID   string   `json:"organization_id"`
	OrgTags []string `json:"organization_tags"`
}

// Apply 把非空身份字段并入凭证。
func (p Profile) Apply(c *Cred) {
	if v := strings.TrimSpace(p.UID); v != "" {
		c.UID = v
	}
	if v := strings.TrimSpace(p.Name); v != "" {
		c.Name = v
	}
	if v := strings.TrimSpace(p.Email); v != "" {
		c.Email = v
	}
	if v := strings.TrimSpace(p.OrgID); v != "" {
		c.OrgID = v
	}
	if len(p.OrgTags) > 0 {
		c.OrgTags = append([]string(nil), p.OrgTags...)
	}
}

// FetchProfile 拉取账号身份（Bearer dt，无 COSY 签名）。
func FetchProfile(oc OAuthConfig, cl *httpClient, dt string) (Profile, error) {
	var p Profile
	status, raw, err := cl.do(http.MethodGet, oc.OpenAPIBase+"/api/v1/userinfo", nil,
		map[string]string{"Accept": "application/json", "Authorization": "Bearer " + dt})
	if err != nil {
		return p, err
	}
	if status != 200 {
		return p, fmt.Errorf("qoder userinfo http %d", status)
	}
	err = json.Unmarshal(raw, &p)
	return p, err
}

// ---------------------------------------------------------------------------
// 时间解析工具（对齐 API-Console 口径）
// ---------------------------------------------------------------------------

// unixSeconds 归一化上游时间戳：毫秒值（>1e11）折算秒——读错会把过期推到
// 数万年后、静默禁用刷新。
func unixSeconds(value int64) time.Time {
	if value > 1e11 {
		value /= 1000
	}
	return time.Unix(value, 0)
}

// parseExpiry 接受 RFC3339(Nano) / "2006-01-02 15:04:05" / 裸 Unix 串（秒或毫秒），
// 或相对寿命 inSeconds。
func parseExpiry(absolute string, inSeconds int64, now time.Time) time.Time {
	if trimmed := strings.TrimSpace(absolute); trimmed != "" {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
			if parsed, err := time.Parse(layout, trimmed); err == nil {
				return parsed
			}
		}
		var value int64
		if _, err := fmt.Sscanf(trimmed, "%d", &value); err == nil && value > 0 {
			return unixSeconds(value)
		}
	}
	if inSeconds != 0 {
		return now.Add(time.Duration(inSeconds) * time.Second)
	}
	return time.Time{}
}

// ---------------------------------------------------------------------------
// 极简 JSON HTTP 客户端（控制面用；SSE 流式走 upstream.Client）
// ---------------------------------------------------------------------------

// controlClient 控制面 HTTP 客户端（30s 超时，连接级错误不重试）。
var controlClient = &http.Client{Timeout: 30 * time.Second}

// do 发一次请求，返回状态码 + 限量 body（64KB）。
func (hc *httpClient) do(method, endpoint string, body []byte, headers map[string]string) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, endpoint, reader)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := hc.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, raw, nil
}

// httpClient 极简 JSON HTTP 客户端（控制面用；流式走 upstream.Client）。
type httpClient struct{ HTTP *http.Client }

func newHTTPClient() *httpClient { return &httpClient{HTTP: controlClient} }
