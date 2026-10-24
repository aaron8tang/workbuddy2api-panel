// login.go QoderWork 设备授权登录（PKCE）：浏览器打开 qoder.com.cn/device/
// selectAccounts 完成登录授权 → 轮询 openapi.qoder.com.cn/api/v1/deviceToken/poll
// 拿设备令牌对（dt-/drt-）→ 落凭证入池。
//
// 端点与参数来源：thirdparty/qoderwork/qoderwork2api-main/internal/server/
// user_api.go（oauthWebsiteCN/oauthClientID/oauthRedirectURI/deviceTokenPoll 实测
// 可用形态）。qoderwork 的 client_id 与 qoder 渠道不同（CN 官网 CLI 应用），
// redirect_uri 仅页面回跳 deep link 用，轮询只认 nonce+verifier。
package qoderwork

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

// CN 侧 OAuth 常量（qoderwork2api 实测：client_id 是 CN 官网 CLI 应用）。
const (
	oauthWebsiteCN = "https://qoder.com.cn"
	oauthOpenapiCN = "https://openapi.qoder.com.cn"
	oauthClientID  = "1c5e33e1-364d-4ce6-b02c-acaa81274a5c"
)

// LoginTTL 授权事务有效期（浏览器侧完成授权的窗口）。
const LoginTTL = 10 * time.Minute

// ErrAuthPending 浏览器侧尚未完成授权（轮询 404/202）。
var ErrAuthPending = errors.New("qoderwork authorization pending")

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

// pollResult 轮询响应（deviceToken/poll 的 token 载荷）。
type pollResult struct {
	Token        string `json:"token"`
	DeviceToken  string `json:"device_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    string `json:"expires_at"`
	ExpiresIn    int64  `json:"expires_in"` // ms
	UserID       string `json:"user_id"`
	UserName     string `json:"user_name"`
}

// PollLogin 轮询设备授权结果。404/202 = ErrAuthPending（浏览器侧未完成）。
// 成功返回凭证（补齐机器指纹与昵称）。
func PollLogin(tx *LoginTransaction) (*Cred, error) {
	if tx == nil {
		return nil, errors.New("qoderwork login transaction is missing")
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
		return nil, fmt.Errorf("qoderwork poll http %d: %.200s", resp.StatusCode, raw)
	}
	var tok pollResult
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, fmt.Errorf("qoderwork poll undecodable: %w", err)
	}
	dt := strings.TrimSpace(tok.Token)
	if dt == "" {
		dt = strings.TrimSpace(tok.DeviceToken)
	}
	if dt == "" {
		return nil, ErrAuthPending // 上游对未完成授权也可能回 200 空 token
	}
	now := time.Now()
	cr := &Cred{
		DT:          dt,
		DRT:         strings.TrimSpace(tok.RefreshToken),
		UID:         strings.TrimSpace(tok.UserID),
		DTExpiresAt: now.Unix(),
	}
	if tok.ExpiresIn > 0 {
		cr.DTExpiresAt = now.Add(time.Duration(tok.ExpiresIn) * time.Millisecond).Unix()
	} else if tok.ExpiresAt != "" {
		if t, perr := time.Parse(time.RFC3339, tok.ExpiresAt); perr == nil {
			cr.DTExpiresAt = t.Unix()
		}
	}
	if cr.DTExpiresAt <= now.Unix() {
		cr.DTExpiresAt = now.Add(30 * 24 * time.Hour).Unix() // dt 观测寿命 30d
	}
	// 身份兜底：poll 不带昵称时用 userinfo 补（失败不阻塞导入）。
	if cr.UID == "" {
		cr.UID = tx.Nonce // 不可得时以事务 nonce 兜底（仍可凭 token 调用）
	}
	if u, err := NewClient().UserInfo(cr.DT); err == nil && u != nil {
		if strings.TrimSpace(u.ID) != "" {
			cr.UID = u.ID
		}
		if strings.TrimSpace(u.Name) != "" {
			cr.Nickname = u.Name
		} else if strings.TrimSpace(u.Username) != "" {
			cr.Nickname = u.Username
		}
	}
	if cr.Nickname == "" {
		cr.Nickname = tok.UserName
	}
	return cr, nil
}
