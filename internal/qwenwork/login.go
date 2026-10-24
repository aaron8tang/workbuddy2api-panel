// login.go 千问办公设备授权登录（PKCE）：浏览器打开 qoder.com.cn/device/
// selectAccounts 完成登录授权 → 轮询 openapi.qoder.com.cn/api/v1/deviceToken/
// poll 拿 JWT + refresh token → 换算为 auth-v2.dat 形态凭证入池。
//
// 依据：CN CLI（qoderclicn）桌面端登录即走 qoder.com.cn 授权页 + CN openapi
// 签发（凭证 JWT 由 openapi 签发、refresh 走 deviceToken/refresh，见 cred.go
// EnsureDT 注释）；本文件把该流程搬到网关侧，替代「桌面端解密 auth-v2.dat 导入」。
// client_id 复用 qoderwork CN CLI 应用（同一 openapi 域签发体系，实测同页授权）。
package qwenwork

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

	"github.com/linguo2625469/workbuddy2api-panel/internal/cosy"
)

// CN 侧 OAuth 常量（与 qoderwork 共用 CN openapi 签发体系）。
const (
	oauthWebsiteCN = "https://qoder.com.cn"
	oauthOpenapiCN = "https://openapi.qoder.com.cn"
	oauthClientID  = "1c5e33e1-364d-4ce6-b02c-acaa81274a5c"
)

// LoginTTL 授权事务有效期。
const LoginTTL = 10 * time.Minute

// ErrAuthPending 浏览器侧尚未完成授权（轮询 404/202 或空 token）。
var ErrAuthPending = errors.New("qwenwork authorization pending")

// LoginTransaction 一次设备授权事务。Verifier 是私有半边，不下发浏览器。
type LoginTransaction struct {
	VerifyURL string    `json:"verify_url"`
	Nonce     string    `json:"nonce"`
	Verifier  string    `json:"verifier"`
	MachineID string    `json:"machine_id"`
	ExpiresAt time.Time `json:"expires_at"`
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
	ver := make([]byte, length)
	for i, b := range raw {
		ver[i] = charset[int(b)%len(charset)]
	}
	verifier = string(ver)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// StartLogin 发起一次设备授权事务（返回浏览器授权 URL）。
func StartLogin() (*LoginTransaction, error) {
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
	query.Set("client_id", oauthClientID)
	query.Set("redirect_uri", "qoder-work-cn://")
	return &LoginTransaction{
		VerifyURL: oauthWebsiteCN + "/device/selectAccounts?" + query.Encode(),
		Nonce:     nonce,
		Verifier:  verifier,
		MachineID: machineID,
		ExpiresAt: time.Now().Add(LoginTTL),
	}, nil
}

// pollResult 轮询响应（deviceToken/poll 的 token 载荷）。CN CLI 签发的
// token 即 cred.JWT（直接当 Bearer 用），refresh_token 对应 cred.RefreshToken。
type pollResult struct {
	Token        string `json:"token"`
	DeviceToken  string `json:"device_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    string `json:"expires_at"`
	ExpiresIn    int64  `json:"expires_in"` // ms
	UserID       string `json:"user_id"`
	UserName     string `json:"user_name"`
}

// PollLogin 轮询设备授权结果。404/202 = ErrAuthPending。
// 成功返回凭证（MachineID 取事务 machine_id，昵称 userinfo 补齐）。
func PollLogin(tx *LoginTransaction) (*Cred, error) {
	if tx == nil {
		return nil, errors.New("qwenwork login transaction is missing")
	}
	query := url.Values{}
	query.Set("nonce", tx.Nonce)
	query.Set("verifier", tx.Verifier)
	query.Set("challenge_method", "S256")
	endpoint := oauthOpenapiCN + "/api/v1/deviceToken/poll?" + query.Encode()
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Go-http-client/2.0")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 || resp.StatusCode == 202 {
		return nil, ErrAuthPending
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("qwenwork poll http %d: %.200s", resp.StatusCode, raw)
	}
	var tok pollResult
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, fmt.Errorf("qwenwork poll undecodable: %w", err)
	}
	token := strings.TrimSpace(tok.Token)
	if token == "" {
		token = strings.TrimSpace(tok.DeviceToken)
	}
	if token == "" {
		return nil, ErrAuthPending
	}
	now := time.Now()
	cr := &Cred{
		Token:        token,
		RefreshToken: strings.TrimSpace(tok.RefreshToken),
		MachineID:    tx.MachineID,
		ExpiresAt:    now.Add(30 * 24 * time.Hour), // 观测兜底，userinfo 后覆盖
	}
	if tok.ExpiresIn > 0 {
		cr.ExpiresAt = now.Add(time.Duration(tok.ExpiresIn) * time.Millisecond)
	} else if tok.ExpiresAt != "" {
		if t, perr := time.Parse(time.RFC3339, tok.ExpiresAt); perr == nil {
			cr.ExpiresAt = t
		}
	}
	// 身份兜底：poll 不带昵称时用 userinfo 补（失败不阻塞导入）。
	cr.UID = strings.TrimSpace(tok.UserID)
	if cr.UID == "" {
		cr.UID = tx.Nonce
	}
	if u, err := NewClient().UserInfo(cr.Token); err == nil && u != nil {
		if strings.TrimSpace(u.ID) != "" {
			cr.UID = u.ID
		}
		if strings.TrimSpace(u.Name) != "" {
			cr.Name = u.Name
		} else if strings.TrimSpace(u.Username) != "" {
			cr.Name = u.Username
		}
		if strings.TrimSpace(u.Email) != "" {
			cr.Email = u.Email
		}
	}
	if cr.Name == "" {
		cr.Name = tok.UserName
	}
	// 运行时字段派生一次并随凭证落盘（与导入路径一致）。
	_, _, _ = cr.EnsureRuntime()
	return cr, nil
}
