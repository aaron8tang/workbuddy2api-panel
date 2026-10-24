// upstream.go 千问办公上游客户端：COSY 签名对话/模型列表 + 控制面 API。
// 控制面（userinfo/plan/quota）挂在网关域（抓包实测：GET /api/v1/userinfo、
// /api/v2/user/plan 用纯 Bearer JWT 打 gateway.qwenwork.cn）。
package qwenwork

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cosy"
)

// Client 上游客户端（HTTP/1.1 强制，与同族网关一致）。
type Client struct {
	HTTP    *http.Client
	Base    string // https://gateway.qwenwork.cn
	OpenAPI string // https://openapi.qoder.com.cn（deviceToken/refresh）
}

// NewClient 生产默认。
func NewClient() *Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	p := cosy.ProfileQwenWork()
	return &Client{
		HTTP:    &http.Client{Timeout: 180 * time.Second, Transport: tr},
		Base:    p.GatewayBase,
		OpenAPI: p.OpenAPIBase,
	}
}

// hostname 本机主机名（cosy-machinehostname，抓包实测 chat 请求携带）。
var hostname = func() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}()

// ChatForward 构造 + 编码 + 签名后发到上游网关。
// 成功返回原始嵌套 SSE 流（调用方 Close）；失败返回 *cosy.Error。
func (c *Client) ChatForward(acct *cosy.Account, cr *Cred, openaiMessages []map[string]any, modelKey string, clientTools []any, reasoningEffort string) (io.ReadCloser, error) {
	if cr.Token == "" {
		return nil, &cosy.Error{Kind: cosy.ErrTokenExpired, Msg: "no access token"}
	}
	rawBody, err := BuildAgentBody(openaiMessages, modelKey, clientTools, reasoningEffort)
	if err != nil {
		return nil, fmt.Errorf("build body: %w", err)
	}
	encoded := cosy.EncodeBody(rawBody)

	profile := cosy.ProfileQwenWork()
	rawURL := c.Base + cosy.ChatPath
	req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	// 运行时字段每账号派生一次并持久复用（抓包实测 Authorization info 恒定）
	key, info, err := cr.EnsureRuntime()
	if err != nil {
		return nil, fmt.Errorf("runtime fields: %w", err)
	}
	sess, err := cosy.NewSessionFromRuntime(acct.Fingerprint, cr.identity(), key, info)
	if err != nil {
		return nil, fmt.Errorf("cosy session: %w", err)
	}
	if err := sess.ApplyHeaders(req, profile, cosy.HeaderParams{
		Body:            encoded,
		RawURL:          rawURL,
		UID:             acct.UID,
		MachineHostname: hostname,
		ModelKey:        modelKey,
		ModelSource:     "system",
		SSE:             true,
	}); err != nil {
		return nil, fmt.Errorf("cosy headers: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, cosy.ClassifyError(resp.StatusCode, truncate(string(raw), 300))
	}
	return resp.Body, nil
}

// DynamicModel 上游 chat 场景模型行。
type DynamicModel struct {
	Key            string   `json:"key"`
	DisplayName    string   `json:"display_name"`
	Enable         bool     `json:"enable"`
	IsReasoning    bool     `json:"is_reasoning"`
	IsVL           bool     `json:"is_vl"`
	IsDefault      bool     `json:"is_default"`
	MaxInputTokens int64    `json:"max_input_tokens"`
	PriceFactor    *float64 `json:"price_factor"`
}

// FetchModels 拉取模型目录（COSY 签名 GET，签名 body 恒为空串）。
func (c *Client) FetchModels(acct *cosy.Account, cr *Cred) ([]DynamicModel, error) {
	if cr.Token == "" {
		return nil, fmt.Errorf("no access token")
	}
	profile := cosy.ProfileQwenWork()
	rawURL := c.Base + cosy.ModelsPath
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	key, info, err := cr.EnsureRuntime()
	if err != nil {
		return nil, fmt.Errorf("runtime fields: %w", err)
	}
	sess, err := cosy.NewSessionFromRuntime(acct.Fingerprint, cr.identity(), key, info)
	if err != nil {
		return nil, fmt.Errorf("cosy session: %w", err)
	}
	if err := sess.ApplyHeaders(req, profile, cosy.HeaderParams{
		RawURL:          rawURL,
		UID:             acct.UID,
		MachineHostname: hostname,
		Accept:          "application/json",
	}); err != nil {
		return nil, fmt.Errorf("cosy headers: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, fmt.Errorf("models api status %d: %.200s", resp.StatusCode, raw)
	}
	var env map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	sceneRaw, ok := env["chat"]
	if !ok {
		var inner map[string]json.RawMessage
		if data, has := env["data"]; has && json.Unmarshal(data, &inner) == nil {
			sceneRaw, ok = inner["chat"]
		}
	}
	if !ok {
		return nil, fmt.Errorf("no chat scene in models response")
	}
	var models []DynamicModel
	if err := json.Unmarshal(sceneRaw, &models); err != nil {
		return nil, fmt.Errorf("chat scene parse: %w", err)
	}
	enabled := make([]DynamicModel, 0, len(models))
	for _, m := range models {
		if m.Enable && m.Key != "" && m.Key != "auto" {
			enabled = append(enabled, m)
		}
	}
	if len(enabled) == 0 {
		return nil, fmt.Errorf("no enabled chat models")
	}
	return enabled, nil
}

// ---------------------------------------------------------------------------
// 控制面 API（Bearer JWT，网关域）
// ---------------------------------------------------------------------------

func (c *Client) get(path, token string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, c.Base+path, nil)
	if err != nil {
		return nil, err
	}
	cosy.ApplyControlHeaders(req, cosy.ProfileQwenWork(), token, false)
	return c.HTTP.Do(req)
}

func readBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// QuotaUsage 查询积分余额（端点随套餐可能缺位——404 视为未知而非错误）。
func (c *Client) QuotaUsage(token string) (remain int64, known bool, err error) {
	resp, err := c.get("/api/v2/quota/usage", token)
	if err != nil {
		return 0, false, err
	}
	raw, err := readBody(resp)
	if err != nil {
		return 0, false, err
	}
	if resp.StatusCode == 404 {
		return 0, false, nil
	}
	if resp.StatusCode >= 400 {
		return 0, false, fmt.Errorf("quota http %d: %.200s", resp.StatusCode, raw)
	}
	var q struct {
		UserQuota struct {
			Remaining float64 `json:"remaining"`
		} `json:"userQuota"`
		AddOnQuota struct {
			Remaining float64 `json:"remaining"`
		} `json:"addOnQuota"`
		IsQuotaExceeded bool `json:"isQuotaExceeded"`
	}
	if err := json.Unmarshal(raw, &q); err != nil {
		return 0, false, fmt.Errorf("quota parse: %w", err)
	}
	return int64(q.UserQuota.Remaining + q.AddOnQuota.Remaining), true, nil
}

// UserPlan 套餐名（auth 文件的 planName 兜底在网关侧处理）。
func (c *Client) UserPlan(token string) (string, error) {
	resp, err := c.get("/api/v2/user/plan", token)
	if err != nil {
		return "", err
	}
	raw, err := readBody(resp)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("plan http %d", resp.StatusCode)
	}
	var p struct {
		UserType     string `json:"user_type"`
		PlanTierName string `json:"plan_tier_name"`
		PlanName     string `json:"planName"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", err
	}
	if p.PlanTierName != "" {
		return p.PlanTierName, nil
	}
	if p.PlanName != "" {
		return p.PlanName, nil
	}
	return p.UserType, nil
}

// UserInfo 用户信息。
type UserInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Username string `json:"username"`
}

// UserInfo 查询用户信息（网关域 /api/v1/userinfo，抓包实测）。
func (c *Client) UserInfo(token string) (*UserInfo, error) {
	resp, err := c.get("/api/v1/userinfo", token)
	if err != nil {
		return nil, err
	}
	raw, err := readBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("userinfo http %d", resp.StatusCode)
	}
	var u UserInfo
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// ResolveModelMap 动态模型 → 客户端模型映射。
func ResolveModelMap(dynamic []DynamicModel) map[string]string {
	out := make(map[string]string, len(dynamic))
	for _, m := range dynamic {
		name := m.Key
		if m.DisplayName != "" {
			name = NormalizeModelName(m.DisplayName)
		}
		out[name] = m.Key
	}
	return out
}

// NormalizeModelName 客户端模型名规范化（小写、空格/下划线转连字符、保留点号）。
func NormalizeModelName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.':
			b.WriteRune(r)
			prevDash = false
		case r == ' ' || r == '_' || r == '-':
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		default:
			b.WriteRune(r)
			prevDash = false
		}
	}
	return strings.Trim(b.String(), "-")
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
