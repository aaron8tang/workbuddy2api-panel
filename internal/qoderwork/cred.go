// Package qoderwork 实现 QoderWork（qoder.com.cn）渠道：
// COSY 签名对话（internal/cosy 共用底座）+ 设备凭证生命周期 + 账号池网关。
// 移植自 thirdparty/qoderwork/qoderwork2api-main（cred/upstream/server），
// 机器指纹统一改由 cosy.Account 持有（落池 state.json）。
package qoderwork

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
)

// Cred 一个账号的设备凭证（dt-/drt-）。
// 机器指纹不在本结构：cosy.Account.Fingerprint 由池持久化，避免双份落盘漂移。
type Cred struct {
	UID      string
	Nickname string

	// DT = 当前 Bearer token（dt-，~30d），DRT = refresh token（drt-，~1y 轮换），
	// DTExpiresAt = dt 过期时刻（unix 秒，0=未知）
	DT          string
	DRT         string
	DTExpiresAt int64

	mu sync.Mutex
}

// AuthInvalidError 凭证已失效（drt refresh 被拒）——需要重新 OAuth 登录。
type AuthInvalidError struct{ Msg string }

func (e *AuthInvalidError) Error() string { return "auth_invalid: " + e.Msg }

// IsAuthInvalid 判断错误是否凭证失效。
func IsAuthInvalid(err error) bool {
	var pe *AuthInvalidError
	return errors.As(err, &pe)
}

// ParseCred 从原始 JSON 字节解析凭证（面板导入用；src 仅用于错误提示）。
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
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
			} `json:"auth"`
			Account struct {
				UID      string `json:"uid"`
				Nickname string `json:"nickname"`
			} `json:"account"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, err
		}
		c = Cred{
			DT:          n.Auth.AccessToken,
			DRT:         n.Auth.RefreshToken,
			DTExpiresAt: n.Auth.ExpiresAt,
			UID:         n.Account.UID,
			Nickname:    n.Account.Nickname,
		}
	} else {
		var f struct {
			Token        string `json:"token"`
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refresh_token"`
			ExpiresAt    int64  `json:"expires_at_unix"`
			UID          string `json:"uid"`
			UserID       string `json:"user_id"`
			Nickname     string `json:"nickname"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, err
		}
		c.DT = f.Token
		if c.DT == "" {
			c.DT = f.AccessToken
		}
		c.DRT = f.RefreshToken
		c.DTExpiresAt = f.ExpiresAt
		c.UID = f.UID
		if c.UID == "" {
			c.UID = f.UserID
		}
		c.Nickname = f.Nickname
	}
	if c.DT == "" && c.DRT == "" {
		return nil, fmt.Errorf("missing dt/drt in %s", where)
	}
	if c.UID == "" {
		// 兜底：从文件名 qoderwork-<uid>.json 提取
		if strings.HasPrefix(filepath.Base(where), "qoderwork-") && strings.HasSuffix(where, ".json") {
			c.UID = strings.TrimSuffix(strings.TrimPrefix(filepath.Base(where), "qoderwork-"), ".json")
		}
	}
	if c.UID == "" {
		return nil, fmt.Errorf("missing uid in %s", where)
	}
	return &c, nil
}

// LoadFile 从磁盘加载 OAuth auth 文件。
// 期望形态（OAuth device flow 落盘）：
//
//	{"auth":{"accessToken":"dt-...","refreshToken":"drt-...","expiresAt":<unix>,"domain":"qoder.com.cn"},
//	 "account":{"uid":"...","nickname":"..."}}
//
// 也兼容扁平形态：{"token":"dt-...","refresh_token":"drt-...","uid":"..."}
func LoadFile(path string) (*Cred, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseCred(raw, path)
}

// LoadDir 扫描目录下 qoderwork*.json，跳过解析失败的。
func LoadDir(dir string) ([]*Cred, error) {
	files, err := filepath.Glob(filepath.Join(dir, "qoderwork*.json"))
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

// FilePath 返回账号凭证的落盘路径。
func (c *Cred) FilePath(dir string) string {
	return filepath.Join(dir, "qoderwork-"+c.UID+".json")
}

// Save 把凭证写回 AuthDir（嵌套形态，面板导入/刷新后持久化）。
func (c *Cred) Save(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(map[string]any{
		"auth": map[string]any{
			"accessToken":  c.DT,
			"refreshToken": c.DRT,
			"expiresAt":    c.DTExpiresAt,
			"domain":       "qoder.com.cn",
		},
		"account": map[string]any{
			"uid":      c.UID,
			"nickname": c.Nickname,
		},
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.FilePath(dir), raw, 0o600)
}

// EnsureDT 确保 DT 有效：余量 >2h 直接用；否则用 DRT 走 deviceToken/refresh。
// refresh 被拒（401/403）→ AuthInvalidError（调用方禁用账号，等重新 OAuth）。
// base 是 openapi 域（https://openapi.qoder.com.cn）。
func (c *Cred) EnsureDT(base string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now().Unix()
	if c.DT != "" && c.DTExpiresAt-now > 7200 {
		return nil
	}
	if c.DRT == "" {
		return &AuthInvalidError{Msg: "no drt available"}
	}
	return c.refreshLocked(base)
}

func (c *Cred) refreshLocked(base string) error {
	body, _ := json.Marshal(map[string]string{"refresh_token": c.DRT})
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/deviceToken/refresh", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return &AuthInvalidError{Msg: fmt.Sprintf("deviceToken refresh http %d", resp.StatusCode)}
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("deviceToken refresh http %d", resp.StatusCode)
	}
	var out struct {
		Token        string `json:"token"`
		DeviceToken  string `json:"device_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    string `json:"expires_at"`
		ExpiresIn    int64  `json:"expires_in"` // ms
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("deviceToken refresh parse: %w", err)
	}
	dt := out.Token
	if dt == "" {
		dt = out.DeviceToken
	}
	if dt == "" || out.RefreshToken == "" {
		return fmt.Errorf("deviceToken refresh: incomplete token pair")
	}
	c.DT = dt
	c.DRT = out.RefreshToken
	now := time.Now()
	if out.ExpiresIn > 0 {
		c.DTExpiresAt = now.Add(time.Duration(out.ExpiresIn) * time.Millisecond).Unix()
	} else if out.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, out.ExpiresAt); err == nil {
			c.DTExpiresAt = t.Unix()
		}
	}
	if c.DTExpiresAt == 0 {
		c.DTExpiresAt = now.Add(30 * 24 * time.Hour).Unix() // dt 观测寿命 30d
	}
	return nil
}
