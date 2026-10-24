// Package autoclaw 智谱 AutoClaw（autoglm）渠道：自定义 MD5 签名头（X-Auth-Sign）
// + X-Authorization 对话链 + refresh token 一次性轮换 + 双模型标识路由
// （X-Request-Model=路由 ID / body.model=剥前缀模型 ID）+ system 提示闸门。
//
// 移植自 agent2api（Rust）providers/autoclaw。国内版与国际版同一套实现按
// Region 参数化（域名/id 前缀不同）；本包只装配国内版（面板配置 region 可扩）。
//
// 关键既成事实（源码实测标注）：
//   - 对话认证头是 X-Authorization: Bearer <token>（不是 Authorization，发错稳定 401）；
//   - 对话链必须不发 X-Harness-Type（发了 403 pay-view/810001）；userapi 链必须带
//     X-Harness-Type: zcode（与 X-Auth-Sign 一起构成官方客户端指纹）——两条链刻意不一致；
//   - 对话链 X-Version: 1.17.8 / userapi 链 1.18.5（目录按它版本门控，过时静默少模型）；
//   - max_tokens ∈ [1,10000) 一律 406 空响应体（预校验墙）：正整数且 <10000 归一到 10000，
//     缺省不注入；max_completion_tokens 不参与校验一字不动；
//   - refresh token 一次性轮换；device_id 是 64 位 hex 不透明串。
package autoclaw

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Region 渠道地区（国内/国际共用一套实现）。
type Region string

const (
	// RegionCN 国内版（本包面板默认装配）。
	RegionCN Region = "autoclaw"
	// RegionIntl 国际版（预留；同一套代码换域名与 id 前缀）。
	RegionIntl Region = "autoclaw-intl"
)

// DomainEndpoints 按 Region 给出两个基址。
type DomainEndpoints struct {
	UserAPI  string // 刷新/积分/签到/登录（不带尾斜杠）
	Upstream string // LLM 代理基址（含尾部 /autoclaw）
}

// Endpoints 各地区域名（source: agent2api region.rs:131-144）。
func Endpoints(r Region) DomainEndpoints {
	if r == RegionIntl {
		return DomainEndpoints{
			UserAPI:  "https://autoglm-api.autoglm.ai",
			Upstream: "https://autoglm-api.autoglm.ai/autoclaw-proxy/proxy/autoclaw",
		}
	}
	return DomainEndpoints{
		UserAPI:  "https://autoglm-acceleration-api.zhipuai.cn",
		Upstream: "https://autoglm-acceleration-api.zhipuai.cn/autoclaw-proxy/proxy/autoclaw",
	}
}

// ExpirySoon 临期窗口 5 分钟（credentials.rs:92）。
const ExpirySoon = 5 * time.Minute

// MaxTokenLength token 最大长度（落库校验，account_store L160-217）。
const MaxTokenLength = 8192

// Credential 一份 AutoClaw 凭据（字段名对齐 account_store 落库形态）。
type Credential struct {
	ID           string `json:"id"`
	Provider     string `json:"provider"` // autoclaw | autoclaw-intl
	Name         string `json:"name,omitempty"`
	UserID       string `json:"userId"`
	DeviceID     string `json:"deviceId,omitempty"`
	Email        string `json:"email,omitempty"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken,omitempty"`
	TokenTail    string `json:"tokenTail,omitempty"`
	// ExpiresAtMs JWT exp × 1000；0 = 未知（unknown 时按不临期处理，由刷新链兜底）。
	ExpiresAtMs int64  `json:"expiresAt,omitempty"`
	Source      string `json:"source,omitempty"` // manual | imported
	Enabled     bool   `json:"enabled"`

	mu       sync.Mutex `json:"-"`
	FilePath string     `json:"-"`
}

// Valid 能拿去对话吗（有 token 即可）。
func (c *Credential) Valid() bool { return strings.TrimSpace(c.AccessToken) != "" }

// CanRefresh 有 refresh token 可续期。
func (c *Credential) CanRefresh() bool { return strings.TrimSpace(c.RefreshToken) != "" }

// NeedsRefresh 临期判定：exp - 5min <= now。exp 未知时不刷（每小时强制刷兜底）。
func (c *Credential) NeedsRefresh(now time.Time) bool {
	if c.ExpiresAtMs <= 0 {
		return false
	}
	return c.ExpiresAtMs-ExpirySoon.Milliseconds() <= now.UnixMilli()
}

// SafeUID 账号 id："user-<userId>"（国内）/ "intl-user-<userId>"（国际）——
// 两地 userId 空间不通，前缀区分。
func (c *Credential) SafeUID() string {
	if c.ID != "" {
		return c.ID
	}
	prefix := "user-"
	if c.Provider == string(RegionIntl) {
		prefix = "intl-user-"
	}
	return prefix + c.UserID
}

// ---------------------------------------------------------------------------
// JWT（仅解 payload，不验签——token 是上游签发，网关只读 claims）
// ---------------------------------------------------------------------------

// jwtClaims 解 JWT payload 段的 claims。
func jwtClaims(token string) (map[string]any, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return nil, errors.New("不是 JWT 形态（缺 payload 段）")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, fmt.Errorf("JWT payload 解码失败: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("JWT payload 不是合法 JSON: %w", err)
	}
	return m, nil
}

// jwtString claim 取字符串（数字按 JS String(x) 处理——user_id 可能是数字）。
func jwtString(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d", int64(v))
		}
		return fmt.Sprintf("%g", v)
	}
	return ""
}

// ParseToken 解析 access token：取 user_id / device_id / exp。
// 必须能解出 user_id，否则拒绝收下（对齐 account_store 的 400）。
func ParseToken(token string) (userID, deviceID string, expiresAtMs int64, err error) {
	claims, err := jwtClaims(token)
	if err != nil {
		return "", "", 0, fmt.Errorf("token 不是有效的 AutoClaw JWT: %w", err)
	}
	userID = jwtString(claims, "user_id")
	if userID == "" {
		return "", "", 0, errors.New("token 不是有效的 AutoClaw JWT（缺 user_id 声明）")
	}
	deviceID = jwtString(claims, "device_id")
	if exp, ok := claims["exp"].(float64); ok && exp > 0 {
		expiresAtMs = int64(exp) * 1000
	}
	return userID, deviceID, expiresAtMs, nil
}

// NewFromToken 从 token 对构建凭据（面板导入/验证码登录共用）。
// token 可为 "enc:" 密文（DPAPI 解密后落明文——见 decryptSafeStorage）。
func NewFromToken(accessToken, refreshToken, deviceID, name, email, source string) (*Credential, error) {
	token := strings.TrimSpace(accessToken)
	token = strings.TrimPrefix(token, "Bearer ")
	if strings.HasPrefix(token, "enc:") {
		plain, err := decryptSafeStorage(strings.TrimPrefix(token, "enc:"))
		if err != nil {
			return nil, err
		}
		token = strings.TrimSpace(plain)
	}
	if refreshToken != "" {
		refreshToken = strings.TrimPrefix(strings.TrimSpace(refreshToken), "Bearer ")
	}
	if token == "" {
		return nil, errors.New("token 为空")
	}
	if len(token) > MaxTokenLength {
		return nil, fmt.Errorf("token 过长（> %d 字符）", MaxTokenLength)
	}
	userID, jwtDevice, exp, err := ParseToken(token)
	if err != nil {
		return nil, err
	}
	if deviceID == "" {
		deviceID = jwtDevice
	}
	return &Credential{
		UserID:       userID,
		DeviceID:     deviceID,
		Name:         strings.TrimSpace(name),
		Email:        strings.TrimSpace(email),
		AccessToken:  token,
		RefreshToken: refreshToken,
		TokenTail:    tokenTail(token),
		ExpiresAtMs:  exp,
		Provider:     string(RegionCN),
		Source:       firstNonEmpty(source, "manual"),
		Enabled:      true,
	}, nil
}

func tokenTail(t string) string {
	if len(t) <= 4 {
		return t
	}
	return t[len(t)-4:]
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 落盘（autoclaw-*.json；与既有渠道 glob 前缀互不相交）
// ---------------------------------------------------------------------------

var uidFileRe = regexp.MustCompile(`[^0-9A-Za-z_-]+`)

// LoadDir 扫描 dir 下的 autoclaw-*.json。
func LoadDir(dir string) ([]*Credential, error) {
	entries, err := filepath.Glob(filepath.Join(dir, "autoclaw-*.json"))
	if err != nil {
		return nil, err
	}
	out := make([]*Credential, 0, len(entries))
	for _, p := range entries {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var c Credential
		if json.Unmarshal(raw, &c) != nil || !c.Valid() {
			continue
		}
		c.FilePath = p
		if c.Provider == "" {
			c.Provider = string(RegionCN)
		}
		out = append(out, &c)
	}
	return out, nil
}

// SaveAtomic 原子写盘（tmp + rename）。
func (c *Credential) SaveAtomic(dir string) error {
	if dir == "" {
		return nil
	}
	c.mu.Lock()
	fp := c.FilePath
	if fp == "" {
		fp = filepath.Join(dir, "autoclaw-"+uidFileRe.ReplaceAllString(c.SafeUID(), "_")+".json")
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

// UpdateTokens 刷新写回：token/refresh 轮换 + 重算 exp。一次性 refresh token
// 刷了不存 = 账号死刑，必须落盘。
func (c *Credential) UpdateTokens(accessToken, refreshToken string, expiresAtMs int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if accessToken != "" {
		c.AccessToken = accessToken
		c.TokenTail = tokenTail(accessToken)
	}
	if refreshToken != "" {
		c.RefreshToken = refreshToken
	}
	if expiresAtMs > 0 {
		c.ExpiresAtMs = expiresAtMs
	}
}
