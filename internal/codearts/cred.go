// Package codearts 华为云 CodeArts 渠道：STS OAuth PKCE + DPoP 登录、
// SDK-HMAC-SHA256 签名出站、3 路并发会话心跳、账号池网关。
//
// 移植自 agent2api（Rust）providers/codearts + login/codearts。关键既成事实：
//   - 凭据是临时 AK/SK/STS（约 24h）+ refresh token + PKCE verifier + DPoP 私钥
//     一整块；少存一半到期后只能重新登录。
//   - refresh token 一次性轮换，续期必须单飞并写回。
//   - 上游对每账号只允许 3 路并发对话：对话前 PUT heartbeat?status=busy 登记
//     自造 user-session-id，结束 idle 注销；并发闸不认请求结束只关心跳。
//   - 模型名大小写敏感（GLM-5.2 ≠ glm-5.2），转发用目录真名。
package codearts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/hwauth"
)

const (
	// ClientID 官方 IDE 插件身份。
	ClientID = "vscode-codebot"
	// TokenURL STS 令牌端点（授权码与 refresh 两个 grant 共用）。
	TokenURL = "https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens"
	// WebLoginBase 授权站点（portal）。
	WebLoginBase = "https://codearts.huaweicloud.com"
	// CallbackPath 回调路径由 portal 固定拼（只认 port）。
	CallbackPath = "/oauth/callback"
	// PKCEMethod 注意：上游写 "SHA-256" 而不是 OAuth 标准的 S256。
	PKCEMethod = "SHA-256"
	// PluginName / PluginVersion 客户端指纹。
	PluginName    = "snap_vscode"
	PluginVersion = "26.9.101"
	// Language 默认语言头。
	Language = "en-us"
	// RefreshLead 提前量 15 分钟。
	RefreshLead = 15 * time.Minute
)

// PkcePair PKCE 两半。verifier 是秘密，续期必须出示当初登录那把。
type PkcePair struct {
	CodeVerifier        string `json:"code_verifier"`
	CodeChallenge       string `json:"code_challenge"`
	CodeChallengeMethod string `json:"code_challenge_method"`
}

// OAuthContext 长期那一半：PKCE + DPoP。
type OAuthContext struct {
	PkcePair   PkcePair       `json:"pkce_pair"`
	DpopKeyPair hwauth.KeyPair `json:"dpop_key_pair"`
}

// Credential 一份 CodeArts 凭据（字段名与上游 JSON 对齐）。
type Credential struct {
	AccessKeyID  string `json:"access_key_id"`
	SecretAccess string `json:"secret_access_key"`
	SecurityTok  string `json:"security_token"`
	// ExpiresAt RFC3339，如 2026-09-27T16:17:00.327Z。
	ExpiresAt string `json:"expires_at"`
	DomainID  string `json:"domain_id"`
	UserID    string `json:"user_id"`
	UserName  string `json:"user_name"`
	LoginType string `json:"login_type"`
	// RefreshToken JWT，一次性轮换。
	RefreshToken string        `json:"refresh_token"`
	OAuthContext *OAuthContext `json:"oauth_context,omitempty"`

	// mu 保护可变字段（token 刷新并发写）。
	mu sync.Mutex `json:"-"`
	// FilePath 落盘路径（LoadDir/SaveAtomic 维护）。
	FilePath string `json:"-"`
}

// Valid AK+SK 齐全可签名。
func (c *Credential) Valid() bool { return c.AccessKeyID != "" && c.SecretAccess != "" }

// clone 深拷贝（锁不复制；OAuthContext 是共享指针，只读字段）。
func (c *Credential) clone() *Credential {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &Credential{
		AccessKeyID:  c.AccessKeyID,
		SecretAccess: c.SecretAccess,
		SecurityTok:  c.SecurityTok,
		ExpiresAt:    c.ExpiresAt,
		DomainID:     c.DomainID,
		UserID:       c.UserID,
		UserName:     c.UserName,
		LoginType:    c.LoginType,
		RefreshToken: c.RefreshToken,
		OAuthContext: c.OAuthContext,
		FilePath:     c.FilePath,
	}
}

// CanRefresh 有 refresh token 且 oauth_context 里 verifier 非空。
func (c *Credential) CanRefresh() bool {
	return strings.TrimSpace(c.RefreshToken) != "" &&
		c.OAuthContext != nil && strings.TrimSpace(c.OAuthContext.PkcePair.CodeVerifier) != ""
}

// ExpiresAtMs 到期毫秒；解析失败时第二个返回值 false（调用方把它当「要刷」）。
func (c *Credential) ExpiresAtMs() (int64, bool) {
	text := strings.TrimSpace(c.ExpiresAt)
	if text == "" {
		return 0, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02T15:04:05Z"} {
		if t, err := time.Parse(layout, text); err == nil {
			return t.UnixMilli(), true
		}
	}
	return 0, false
}

// NeedsRefresh STS 临期判定：无 token / 无到期时间不刷；解析不出来当作要刷
//（坏时间戳绝不能被当成新鲜）。
func (c *Credential) NeedsRefresh(lead time.Duration, now time.Time) bool {
	if c.SecurityTok == "" || !c.Valid() || c.ExpiresAt == "" {
		return false
	}
	expiry, ok := c.ExpiresAtMs()
	if !ok {
		return true
	}
	return expiry <= now.Add(lead).UnixMilli()
}

// Signing 组装签名凭据（domainless 由调用方清 DomainID）。
func (c *Credential) Signing() hwauth.SigningCredential {
	return hwauth.SigningCredential{
		AccessKeyID: c.AccessKeyID, SecretKey: c.SecretAccess,
		SecurityToken: c.SecurityTok, DomainID: c.DomainID,
	}
}

// Identity 同账号判定串（domain+user 优先，退 user_name / AK）。
func (c *Credential) Identity() string {
	if c.DomainID != "" || c.UserID != "" {
		return c.DomainID + "\x00" + c.UserID
	}
	if c.UserName != "" {
		return c.UserName
	}
	return c.AccessKeyID
}

// ParseCred 导入解析：先找 codearts_provider_credential / credential 包裹键，
// 回退平铺；到期别名 expiration/expiresAt；身份别名 account_id/principal_id。
func ParseCred(raw []byte) (*Credential, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("CodeArts 凭据不是合法 JSON: %w", err)
	}
	root := body
	for _, key := range []string{"codearts_provider_credential", "credential"} {
		if inner, ok := body[key]; ok {
			if err := json.Unmarshal(inner, &root); err != nil {
				return nil, fmt.Errorf("CodeArts 凭据包裹段解析失败: %w", err)
			}
			break
		}
	}
	var c Credential
	if err := unmarshalLoose(root, &c); err != nil {
		return nil, fmt.Errorf("CodeArts 凭据解析失败: %w", err)
	}
	if c.ExpiresAt == "" {
		c.ExpiresAt = textOf(root, "expiration", "expires_at", "expiresAt")
	}
	if c.DomainID == "" {
		c.DomainID = textOf(root, "domain_id", "account_id")
	}
	if c.UserID == "" {
		c.UserID = textOf(root, "user_id", "principal_id")
	}
	if c.UserName == "" {
		c.UserName = textOf(root, "user_name", "principal_urn")
	}
	if c.LoginType == "" {
		c.LoginType = textOf(root, "login_type")
		if c.LoginType == "" {
			c.LoginType = "WEB"
		}
	}
	if !c.Valid() {
		return nil, errors.New("CodeArts 凭据缺少 access_key_id 或 secret_access_key")
	}
	return &c, nil
}

// unmarshalLoose 忽略未知字段解析。
func unmarshalLoose(raw any, out any) error {
	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	return dec.Decode(out)
}

// textOf 取第一个非空字符串字段。
func textOf(m map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err == nil && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 凭证目录（codearts-<uid>.json，嵌套形 {"auth":{...}} 与平铺均可）
// ---------------------------------------------------------------------------

// LoadDir 扫描 dir 下的 codearts-*.json。
func LoadDir(dir string) ([]*Credential, error) {
	entries, err := filepath.Glob(filepath.Join(dir, "codearts-*.json"))
	if err != nil {
		return nil, err
	}
	out := make([]*Credential, 0, len(entries))
	for _, p := range entries {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		// 嵌套形态 {"auth": {...}} 优先，平铺回退。
		var wrapper struct {
			Auth *json.RawMessage `json:"auth"`
		}
		payload := raw
		if json.Unmarshal(raw, &wrapper) == nil && wrapper.Auth != nil {
			payload = *wrapper.Auth
		}
		cr, err := ParseCred(payload)
		if err != nil || cr.UserName == "" && cr.UserID == "" && cr.AccessKeyID == "" {
			continue
		}
		cr.FilePath = p
		out = append(out, cr)
	}
	return out, nil
}

// uidRe 账号 uid：user_name/user_id 归一成文件安全串。
var uidRe = regexp.MustCompile(`[^0-9A-Za-z_.-]+`)

// SafeUID 由凭据派生稳定 uid（user_id 优先，退 user_name，最后 AK 前 12 位）。
func (c *Credential) SafeUID() string {
	name := c.UserID
	if name == "" {
		name = c.UserName
	}
	if name == "" {
		name = c.AccessKeyID
	}
	name = uidRe.ReplaceAllString(name, "_")
	if name == "" {
		name = "codearts"
	}
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// SaveAtomic 原子写盘（tmp + rename）。
func (c *Credential) SaveAtomic(dir string) error {
	if dir == "" {
		return nil
	}
	c.mu.Lock()
	fp := c.FilePath
	if fp == "" {
		fp = filepath.Join(dir, "codearts-"+c.SafeUID()+".json")
		c.FilePath = fp
	}
	c.mu.Unlock()
	wrapper := map[string]any{"auth": c}
	raw, err := json.MarshalIndent(wrapper, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := fp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, fp)
}
