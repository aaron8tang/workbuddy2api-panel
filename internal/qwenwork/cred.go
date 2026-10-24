// Package qwenwork 实现千问办公（gateway.qwenwork.cn）渠道：
// COSY 签名对话（internal/cosy 共用底座，ProfileQwenWork 变体）+
// 桌面端凭证导入（解密 auth-v2.dat 形态）+ 每账号持久运行时字段。
//
// 协议证据来源：本地千问办公客户端（qoderclicn 1.1.59）抓包实测——
// product=cli / clienttype=5 / scene=assistant / clientip=machineid /
// machinetoken=machineid / 运行时字段跨请求复用（Authorization info 恒定）。
// JWT 刷新走 CN CLI 复用的开放接口 openapi.qoder.com.cn/api/v1/deviceToken/refresh。
package qwenwork

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cosy"
)

// RefreshLead 刷新提前量。
const RefreshLead = 30 * time.Minute

// AuthInvalidError 凭证已失效（refresh 被拒）——需重新从桌面端导入。
type AuthInvalidError struct{ Msg string }

func (e *AuthInvalidError) Error() string { return "auth_invalid: " + e.Msg }

// IsAuthInvalid 判断错误是否凭证失效。
func IsAuthInvalid(err error) bool {
	var pe *AuthInvalidError
	return errors.As(err, &pe)
}

// Cred 一个账号的凭证 + 派生运行时字段。
// 形态对齐桌面端解密 auth-v2.dat：token=JWT / refreshToken=ory_rt_* / user 元数据。
type Cred struct {
	UID      string // user.id
	Name     string // user.name
	Email    string // user.email
	Username string // user.username

	Token         string // JWT access token
	RefreshToken  string
	ExpiresAt     time.Time
	RefreshExpiry time.Time

	// 机器身份：桌面端 CLI 自有 machine id（与 loginDeviceId 无关），
	// machinetoken 对齐实测恒等 machineid。
	MachineID string

	// 派生运行时字段（等价 cosy.Session 的 CosyKey/Info），抓包实测跨请求复用。
	RuntimeKey  string
	RuntimeInfo string

	mu sync.Mutex
}

// HasCredential 是否带任何可用凭证。
func (c *Cred) HasCredential() bool {
	return strings.TrimSpace(c.Token) != "" || strings.TrimSpace(c.RefreshToken) != ""
}

// AccessValid 报告 JWT 在 now 时刻是否可用（exp 内且余量 > RefreshLead）。
func (c *Cred) AccessValid(now time.Time) bool {
	if strings.TrimSpace(c.Token) == "" {
		return false
	}
	if c.ExpiresAt.IsZero() {
		return true
	}
	return c.ExpiresAt.Sub(now) > RefreshLead
}

// identity 组装 COSY identity（千问办公侧 user_type 为空——JWT 身份无账号类别）。
func (c *Cred) identity() cosy.Identity {
	return cosy.Identity{
		Name:               c.Name,
		Aid:                c.UID,
		UID:                c.UID,
		SecurityOAuthToken: c.Token,
		RefreshToken:       c.RefreshToken,
	}
}

// EnsureRuntime 返回可用的运行时字段（已派生则复用，与抓包行为一致）。
func (c *Cred) EnsureRuntime() (key, info string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.RuntimeKey != "" && c.RuntimeInfo != "" && c.Token != "" {
		return c.RuntimeKey, c.RuntimeInfo, nil
	}
	key, info, err = cosy.DeriveRuntime(c.identity())
	if err != nil {
		return "", "", err
	}
	c.RuntimeKey, c.RuntimeInfo = key, info
	return key, info, nil
}

// InvalidateRuntime token 轮换后清空运行时字段。
func (c *Cred) InvalidateRuntime() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.RuntimeKey, c.RuntimeInfo = "", ""
}

// fingerprint 账号机器指纹（machinetoken=machineid，与抓包一致）。
func (c *Cred) fingerprint() cosy.Fingerprint {
	return cosy.Fingerprint{MachineID: c.MachineID, MachineToken: c.MachineID, MachineType: "5"}
}

// completeFingerprint 无 MachineID 的导入凭证补齐。
func (c *Cred) completeFingerprint() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(c.MachineID) == "" {
		c.MachineID = cosy.UUID()
	}
}

// ---------------------------------------------------------------------------
// 落盘（qwenwork-<uid>.json）
// ---------------------------------------------------------------------------

// ParseCred 从原始 JSON 解析凭证（src 仅用于错误提示与文件名兜底）。
func ParseCred(raw []byte, src ...string) (*Cred, error) {
	where := ""
	if len(src) > 0 {
		where = src[0]
	}
	var f struct {
		SchemaVersion int    `json:"schemaVersion"`
		Token         string `json:"token"`
		AccessToken   string `json:"accessToken"`
		RefreshToken  string `json:"refreshToken"`
		RefreshToken2 string `json:"refresh_token"`
		ExpiresAt     string `json:"expiresAt"`
		ExpiresAtUnix int64  `json:"expires_at_unix"`
		RefreshExpiry string `json:"refreshTokenExpiresAt"`
		User          struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Email    string `json:"email"`
			Username string `json:"username"`
		} `json:"user"`
		UID           string `json:"uid"`
		UserID        string `json:"user_id"`
		MachineID     string `json:"machine_id"`
		LoginDeviceID string `json:"loginDeviceId"`
		Runtime       struct {
			Key  string `json:"key"`
			Info string `json:"info"`
		} `json:"runtime"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	c := &Cred{
		Token:        f.Token,
		RefreshToken: f.RefreshToken,
		MachineID:    f.MachineID,
		UID:          f.User.ID,
		Name:         f.User.Name,
		Email:        f.User.Email,
		Username:     f.User.Username,
		RuntimeKey:   f.Runtime.Key,
		RuntimeInfo:  f.Runtime.Info,
	}
	if c.Token == "" {
		c.Token = f.AccessToken
	}
	if c.RefreshToken == "" {
		c.RefreshToken = f.RefreshToken2
	}
	if t, err := time.Parse(time.RFC3339, f.ExpiresAt); err == nil {
		c.ExpiresAt = t
	} else if f.ExpiresAtUnix > 0 {
		if f.ExpiresAtUnix > 1e11 {
			f.ExpiresAtUnix /= 1000
		}
		c.ExpiresAt = time.Unix(f.ExpiresAtUnix, 0)
	}
	if t, err := time.Parse(time.RFC3339, f.RefreshExpiry); err == nil {
		c.RefreshExpiry = t
	}
	if c.UID == "" {
		c.UID = f.UserID
	}
	if c.UID == "" {
		c.UID = f.UID
	}
	if c.UID == "" {
		c.UID = f.LoginDeviceID // 最后兜底：设备号（无身份元数据时仍可调用）
	}
	if !c.HasCredential() {
		return nil, fmt.Errorf("missing token/refreshToken in %s", where)
	}
	if c.UID == "" {
		base := filepath.Base(where)
		if strings.HasPrefix(base, "qwenwork-") && strings.HasSuffix(base, ".json") {
			c.UID = strings.TrimSuffix(strings.TrimPrefix(base, "qwenwork-"), ".json")
		}
	}
	if c.UID == "" {
		return nil, fmt.Errorf("missing uid in %s", where)
	}
	return c, nil
}

// LoadFile 从磁盘加载凭证。
func LoadFile(path string) (*Cred, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseCred(raw, path)
}

// LoadDir 扫描目录下 qwenwork-*.json，跳过解析失败的。
func LoadDir(dir string) ([]*Cred, error) {
	files, err := filepath.Glob(filepath.Join(dir, "qwenwork-*.json"))
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
	return filepath.Join(dir, "qwenwork-"+c.UID+".json")
}

// Save 凭证写盘（对齐 auth-v2.dat 形态 + runtime 扩展字段）。
func (c *Cred) Save(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	c.mu.Lock()
	raw, err := json.MarshalIndent(map[string]any{
		"schemaVersion": 2,
		"token":         c.Token,
		"refreshToken":  c.RefreshToken,
		"expiresAt":     c.ExpiresAt.Format(time.RFC3339),
		"refreshTokenExpiresAt": func() string {
			if c.RefreshExpiry.IsZero() {
				return ""
			}
			return c.RefreshExpiry.Format(time.RFC3339)
		}(),
		"user": map[string]any{
			"id":       c.UID,
			"name":     c.Name,
			"email":    c.Email,
			"username": c.Username,
		},
		"machine_id": c.MachineID,
		"runtime":    map[string]any{"key": c.RuntimeKey, "info": c.RuntimeInfo},
	}, "", "  ")
	c.mu.Unlock()
	if err != nil {
		return err
	}
	return os.WriteFile(c.FilePath(dir), raw, 0o600)
}

// EnsureDT 保证 JWT 可用（余量 > RefreshLead 直接用；否则 deviceToken/refresh）。
// openapiBase = CN CLI 复用的开放接口底座（openapi.qoder.com.cn）。
func (c *Cred) EnsureDT(openapiBase string) error {
	c.mu.Lock()
	if c.AccessValid(time.Now()) {
		c.mu.Unlock()
		return nil
	}
	drt := strings.TrimSpace(c.RefreshToken)
	if drt == "" {
		c.mu.Unlock()
		return &AuthInvalidError{Msg: "no refresh token; re-import from desktop app"}
	}
	body, _ := json.Marshal(map[string]string{"refresh_token": drt})
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest(http.MethodPost, openapiBase+"/api/v1/deviceToken/refresh", bytes.NewReader(body))
	if err != nil {
		c.mu.Unlock()
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		c.mu.Unlock()
		return &AuthInvalidError{Msg: fmt.Sprintf("deviceToken refresh http %d", resp.StatusCode)}
	}
	if resp.StatusCode >= 400 {
		c.mu.Unlock()
		return fmt.Errorf("deviceToken refresh http %d", resp.StatusCode)
	}
	var out struct {
		Token        string `json:"token"`
		DeviceToken  string `json:"device_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    string `json:"expires_at"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		c.mu.Unlock()
		return fmt.Errorf("deviceToken refresh parse: %w", err)
	}
	dt := out.Token
	if dt == "" {
		dt = out.DeviceToken
	}
	if dt == "" {
		c.mu.Unlock()
		return &AuthInvalidError{Msg: "refresh response carried no token"}
	}
	c.Token = dt
	if rt := strings.TrimSpace(out.RefreshToken); rt != "" {
		c.RefreshToken = rt
	}
	if out.ExpiresIn > 0 {
		c.ExpiresAt = time.Now().Add(time.Duration(out.ExpiresIn) * time.Millisecond)
	} else if t, err := time.Parse(time.RFC3339, out.ExpiresAt); err == nil {
		c.ExpiresAt = t
	}
	c.RuntimeKey, c.RuntimeInfo = "", "" // 重派生
	c.mu.Unlock()
	return nil
}
