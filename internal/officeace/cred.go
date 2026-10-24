// Package officeace 华为云果办（OfficeClaw / jiuwenclaw）渠道：上游本身就是
// OpenAI 兼容网关（Basic auth，不过期），OAuth PKCE+DPoP 链换来的临时 AK/SK
// 只服务控制面（订阅/倍率/续期/开通）。签名用与 CodeArts 同名但有三处实测
// 差异的 SDK/V11-HMAC-SHA256（见 internal/hwauth 包注释）。
//
// 移植自 agent2api（Rust）providers/officeace。
package officeace

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
	// ClientID IAM OAuth2 客户端。
	ClientID = "pdp5_for_agentarts"
	// CloudBase AgentArts 云端控制面。
	CloudBase = "https://agentarts.cn-southwest-2.myhuaweicloud.com"
	// AuthBase 华为云统一登录。
	AuthBase = "https://auth.huaweicloud.com"
	// RedirectURI 指向云端（非 loopback）——授权码靠轮询获取，桌面壳无需开回调监听。
	RedirectURI = CloudBase + "/v1/claw/auth/callback"
	// TokenURL STS 令牌端点（两 grant 共用）。
	TokenURL = "https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens"
	// Region 控制面与网关同区域。
	Region = "cn-southwest-2"
	// PollInterval 登录取码轮询间隔。
	PollInterval = 2500 * time.Millisecond
	// ExpiryMargin 控制面临期窗口（实测 AK/SK 活约 2 小时）。
	ExpiryMargin = 30 * time.Minute
)

// Credential 两层凭据（别混）：
//   - 网关凭据（转发用）：modelAppKey:Secret → Basic 头，不过期；
//   - 控制面临时凭据：HSTA… AK/SK + STS + project_id，实测约 2 小时。
type Credential struct {
	ID            string `json:"id,omitempty"`
	BaseURL       string `json:"baseUrl"`
	ModelAppKey   string `json:"modelAppKey"`
	ModelAppSecret string `json:"modelAppSecret"`
	AccessKeyID   string `json:"accessKeyId,omitempty"`
	SecretAccess  string `json:"secretAccessKey,omitempty"`
	SecurityTok   string `json:"securityToken,omitempty"`
	ProjectID     string `json:"projectId,omitempty"`
	// ExpiresAtMs 控制面到期毫秒（0=未知）。
	ExpiresAtMs int64 `json:"expiresAt,omitempty"`
	// RefreshToken 一次性轮换；续期链的唯一钥匙。
	RefreshToken string         `json:"refreshToken,omitempty"`
	DpopKeyPair  hwauth.KeyPair `json:"dpopKeyPair,omitempty"`
	// Nickname 账号显示名（面板用）。
	Nickname string `json:"name,omitempty"`

	// mu 保护可变字段。
	mu       sync.Mutex `json:"-"`
	FilePath string     `json:"-"`
}

// CanForward 转发可用的最低条件：基址 + 一对网关凭据。
func (c *Credential) CanForward() bool {
	return c.BaseURL != "" && c.ModelAppKey != "" && c.ModelAppSecret != ""
}

// HasControlPlane 控制面凭据齐不齐（额度/签到/续期要用）。
func (c *Credential) HasControlPlane() bool {
	return c.AccessKeyID != "" && c.SecretAccess != ""
}

// ControlPlaneExpiring 到期前 ExpiryMargin 算临期。
func (c *Credential) ControlPlaneExpiring() bool {
	return c.ExpiresAtMs > 0 && c.ExpiresAtMs-ExpiryMargin.Milliseconds() <= time.Now().UnixMilli()
}

// Signing 控制面签名凭据。
func (c *Credential) Signing() hwauth.SigningCredential {
	return hwauth.SigningCredential{
		AccessKeyID: c.AccessKeyID, SecretKey: c.SecretAccess,
		SecurityToken: c.SecurityTok, ProjectID: c.ProjectID,
	}
}

// SafeUID 由 baseUrl+appKey FNV-1a 派生稳定 id（同凭据重复导入落同一条）。
func (c *Credential) SafeUID() string {
	if c.ID != "" {
		return c.ID
	}
	h := fnv1a64([]byte(normalizeBase(c.BaseURL) + "|" + strings.TrimSpace(c.ModelAppKey)))
	return "officeace-" + fmt.Sprintf("%012x", h)
}

func fnv1a64(b []byte) uint64 {
	h := uint64(0xcbf29ce484222325)
	for _, c := range b {
		h ^= uint64(c)
		h *= 0x00000100000001b3
	}
	return h
}

// uidRe 文件安全串。
var uidRe = regexp.MustCompile(`[^0-9A-Za-z_-]+`)

// NormalizeBase 网关基址规范到 https://<host>/v2。
func normalizeBase(baseURL string) string {
	t := strings.Trim(strings.TrimSpace(baseURL), "/")
	if t == "" {
		return ""
	}
	t = strings.TrimPrefix(strings.TrimPrefix(t, "https://"), "http://")
	t = strings.Trim(t, "/")
	if t == "" {
		return ""
	}
	if strings.HasSuffix(t, "/v2") {
		return "https://" + t
	}
	return "https://" + t + "/v2"
}

// ChatEndpoint {base}/chat/completions。
func ChatEndpoint(baseURL string) string {
	b := normalizeBase(baseURL)
	if b == "" {
		return ""
	}
	return b + "/chat/completions"
}

// ParseCred 导入解析（键别名对齐 agent2api credentials.rs from_record）。
func ParseCred(raw []byte) (*Credential, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("OfficeAce 凭据不是合法 JSON: %w", err)
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			var s string
			if v, ok := m[k]; ok && json.Unmarshal(v, &s) == nil && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
		return ""
	}
	c := &Credential{
		BaseURL:        pick("baseUrl", "base_url", "api_base", "apiBase"),
		ModelAppKey:    pick("modelAppKey", "appKey", "model_app_key"),
		ModelAppSecret: pick("modelAppSecret", "appSecret", "model_app_secret"),
		AccessKeyID:    pick("accessKeyId", "access_key_id", "accessKey"),
		SecretAccess:   pick("secretAccessKey", "secret_access_key", "secretKey"),
		SecurityTok:    pick("securityToken", "security_token"),
		ProjectID:      pick("projectId", "project_id"),
		RefreshToken:   pick("refreshToken", "refresh_token"),
		Nickname:       pick("name", "nickname"),
	}
	var expires int64
	if v, ok := m["expiresAt"]; ok {
		var n int64
		if json.Unmarshal(v, &n) == nil {
			expires = n
		} else {
			var f float64
			if json.Unmarshal(v, &f) == nil {
				expires = int64(f)
			}
		}
	}
	c.ExpiresAtMs = expires
	if v, ok := m["dpopKeyPair"]; ok {
		_ = json.Unmarshal(v, &c.DpopKeyPair)
	}
	if !c.CanForward() {
		return nil, errors.New("OfficeAce 账号缺少网关凭据（需要 baseUrl 与 modelAppKey/modelAppSecret 两项）")
	}
	return c, nil
}

// LoadDir 扫描 officeace-*.json。
func LoadDir(dir string) ([]*Credential, error) {
	entries, err := filepath.Glob(filepath.Join(dir, "officeace-*.json"))
	if err != nil {
		return nil, err
	}
	out := make([]*Credential, 0, len(entries))
	for _, p := range entries {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		cr, err := ParseCred(raw)
		if err != nil {
			continue
		}
		cr.FilePath = p
		if cr.ID == "" {
			base := strings.TrimSuffix(filepath.Base(p), ".json")
			cr.ID = uidRe.ReplaceAllString(strings.TrimPrefix(base, "officeace-"), "_")
		}
		out = append(out, cr)
	}
	return out, nil
}

// SaveAtomic 原子写盘。
func (c *Credential) SaveAtomic(dir string) error {
	if dir == "" {
		return nil
	}
	c.mu.Lock()
	fp := c.FilePath
	if fp == "" {
		fp = filepath.Join(dir, c.SafeUID()+".json")
		c.FilePath = fp
	}
	c.mu.Unlock()
	raw, err := json.MarshalIndent(c, "", "  ")
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

// UpdateControlPlane 续期写回：只改控制面四字段 + 到期 + 轮换后的 refresh token；
// 不碰 baseUrl / modelAppKey / dpopKeyPair。
func (c *Credential) UpdateControlPlane(ak, sk, st, pid string, expiresAtMs int64, nextRefresh string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ak != "" {
		c.AccessKeyID = ak
	}
	if sk != "" {
		c.SecretAccess = sk
	}
	if st != "" {
		c.SecurityTok = st
	}
	if pid != "" {
		c.ProjectID = pid
	}
	if expiresAtMs > 0 {
		c.ExpiresAtMs = expiresAtMs
	}
	if nextRefresh != "" {
		c.RefreshToken = nextRefresh
	}
}
