// upstream.go Qoder 上游客户端：COSY 签名对话/模型列表。
// 移植自 API-Console internal/qoder/{client,request_transport,models_upstream}.go +
// qoder2api models.py 的目录解析。错误分类叠加 Qoder 特有三态：
//   - 订阅缺失（HTTP 200 SSE 包裹业务 403 + qoder.com/pricing 文案）
//     ——绝不能当认证失败禁用账号
//   - billing daily count exceeded（日额度耗尽，等次日重置）
//   - 10605 共享队列拒答（busy，重试其他账号）
package qoder

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cosy"
)

// ErrNoEntitlement 账号无当前套餐权益（不可禁用账号，等套餐恢复/更换账号）。
var ErrNoEntitlement = errors.New("qoder account has no active entitlement for this model")

// ErrDailyCountExceeded 账号当日用量达上限（次日重置）。
var ErrDailyCountExceeded = errors.New("qoder billing daily count exceeded")

// Client 上游客户端（流式长连接不带总超时，空闲由调用方 ctx 约束）。
type Client struct {
	HTTP    *http.Client
	Base    string // https://api2.qoder.sh（推理网关）
	OpenAPI string // https://openapi.qoder.sh
	OAuth   OAuthConfig
}

// NewClient 生产默认。
func NewClient() *Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{}, // 强制 HTTP/1.1
	}
	profile := cosy.ProfileQoder()
	oc := OAuthConfig{
		OAuthBase:     profile.OAuthBase,
		OpenAPIBase:   profile.OpenAPIBase,
		ClientID:      profile.ClientID,
		ClientVersion: profile.CosyVersion,
	}
	return &Client{
		HTTP:    &http.Client{Transport: tr},
		Base:    profile.GatewayBase,
		OpenAPI: profile.OpenAPIBase,
		OAuth:   oc,
	}
}

// ChatForward 构造 + 编码 + 签名后发到上游推理网关。
// 成功返回原始嵌套 SSE 流（调用方 Close）；失败返回分类后的错误。
//
// 模型排队协议（逆向 Qoder CN 桌面端 worker，qoder-worker-runtime.obf.mjs）：
// 上游容量不足时不拒绝，而是返回流内 10605 错误帧（isQueued:true, queueType:"p4",
// retryAfterSeconds:30, serviceAvailable:false），同时为请求体里的 request_set_id
// 建一个 lease；桌面端随后轮询
//
//	GET /algo/api/v2/service/ask/queue/status?requestSetId=&modelKey=&queueType=
//
// 直到 isQueued:false / serviceAvailable:true（出队），再以**同一 request_set_id**
// 重发聊天请求（request_id/chat_record_id 刷新），结束后上报
//
//	POST /algo/api/v2/service/ask/finish?Encode=1（time_consumed，失败无害，服务端 TTL 兜底释放）
//
// 本实现复刻该流程：peek 流首帧判定 10605 → 排队轮询 → 出队重发（≤3 次），
// 全程尊重调用方 ctx（客户端断连立即中止）。等待预算 queueMaxWait 封顶。
func (c *Client) ChatForward(ctx context.Context, acct *cosy.Account, cr *Cred, opts ChatOptions) (io.ReadCloser, error) {
	if cr.DT == "" {
		return nil, &cosy.Error{Kind: cosy.ErrTokenExpired, Msg: "no access token"}
	}
	if opts.RequestID == "" {
		opts.RequestID = cosy.UUID()
	}
	if opts.RequestSetID == "" {
		opts.RequestSetID = cosy.UUID()
	}
	if opts.SessionID == "" {
		opts.SessionID = conversationSessionID(acct.UID, opts.RequestID)
	}
	if opts.BusinessProduct == "" {
		opts.BusinessProduct = "qoder_work"
	}
	if opts.ClientVersion == "" {
		opts.ClientVersion = c.OAuth.ClientVersion
	}

	deadline := time.Now().Add(queueMaxWait)
	for attempt := 0; ; attempt++ {
		rc, err := c.doChat(ctx, acct, cr, opts)
		if err != nil {
			return nil, err
		}
		peek, perr := peekQueueFrame(rc)
		if perr != nil {
			rc.Close()
			return nil, perr
		}
		if peek.queued == nil {
			// 首帧非 10605：把已读字节还回流头，正常转发
			return peek.rejoin(rc), nil
		}
		rc.Close()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := c.waitForQueue(ctx, acct, cr, opts, peek.queued, deadline); err != nil {
			return nil, err
		}
		if attempt+1 >= queueMaxResend {
			return nil, &cosy.Error{Kind: cosy.ErrBusy, Msg: "queue drained but resend budget exhausted (request_set_id=" + opts.RequestSetID + ")"}
		}
		// 出队重发：同 request_set_id（lease 放行），request_id/chat_record_id 刷新
		opts.RequestID = ""
	}
}

// doChat 单次聊天请求（构造 + 编码 + 签名 + 发送）。
func (c *Client) doChat(ctx context.Context, acct *cosy.Account, cr *Cred, opts ChatOptions) (io.ReadCloser, error) {
	rawBody, err := BuildChatBody(opts)
	if err != nil {
		return nil, fmt.Errorf("build body: %w", err)
	}
	encoded := cosy.EncodeBody(rawBody)

	profile := cosy.ProfileQoder()
	rawURL := c.Base + cosy.ChatPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	// Qoder 侧运行时字段每账号派生一次并持久复用（CLI 同款行为）
	key, info, err := cr.EnsureRuntime()
	if err != nil {
		return nil, fmt.Errorf("runtime fields: %w", err)
	}
	sess, err := cosy.NewSessionFromRuntime(acct.Fingerprint, cr.identity(), key, info)
	if err != nil {
		return nil, fmt.Errorf("cosy session: %w", err)
	}
	if err := sess.ApplyHeaders(req, profile, cosy.HeaderParams{
		Body:        encoded,
		RawURL:      rawURL,
		UID:         acct.UID,
		OrgID:       cr.OrgID,
		OrgTags:     cr.OrgTags,
		ModelKey:    opts.Model.Key,
		ModelSource: firstNonEmpty(opts.Model.Source, "system"),
		RequestID:   opts.RequestID,
		SSE:         true,
	}); err != nil {
		return nil, fmt.Errorf("cosy headers: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		resp.Body.Close()
		return nil, classifyQoder(resp.StatusCode, raw)
	}
	return resp.Body, nil
}

// FetchModels 拉取上游模型目录（COSY 签名 GET，签名 body 恒为空串）。
// 返回 chat 场景可用行（剔除 auto 路由入口与禁用行）。
func (c *Client) FetchModels(acct *cosy.Account, cr *Cred) ([]modelEntry, error) {
	if cr.DT == "" {
		return nil, fmt.Errorf("no access token")
	}
	profile := cosy.ProfileQoder()
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
		RawURL:  rawURL,
		UID:     acct.UID,
		OrgID:   cr.OrgID,
		OrgTags: cr.OrgTags,
		Accept:  "application/json",
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
	// 响应为明文 JSON：{"chat":[...],"developer":[...],...}；若套 data 包则拆一层
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
	var models []modelEntry
	if err := json.Unmarshal(sceneRaw, &models); err != nil {
		return nil, fmt.Errorf("chat scene parse: %w", err)
	}
	enabled := make([]modelEntry, 0, len(models))
	for _, m := range models {
		// auto 是路由入口而非真实模型；无 key/display 的行不可选
		if !m.enabled() || m.Key == "" || m.Key == "auto" {
			continue
		}
		enabled = append(enabled, m)
	}
	if len(enabled) == 0 {
		return nil, fmt.Errorf("no enabled chat models")
	}
	return enabled, nil
}

// ---------------------------------------------------------------------------
// 错误分类（Qoder 特化叠加在 cosy.Classify 之上）
// ---------------------------------------------------------------------------

// classifyQoder HTTP 失败分类。10605 队列拒答以业务码出现在 401 下，
// 必须先于状态码判定（busy 时刷新 token 纯属浪费）。
// 订阅缺失/日额度两类返回包级哨兵（errors.Is 判定）——网关侧据此冷却而非禁用。
func classifyQoder(status int, raw []byte) error {
	body := string(raw)
	if strings.Contains(body, "qoder.com/pricing") {
		return fmt.Errorf("%w (http %d): %s", ErrNoEntitlement, status, firstLine(body))
	}
	if strings.Contains(strings.ToLower(body), "billing daily count exceeded") {
		return fmt.Errorf("%w (http %d): %s", ErrDailyCountExceeded, status, firstLine(body))
	}
	if strings.Contains(body, "10605") ||
		strings.Contains(body, `"isqueued":true`) ||
		strings.Contains(body, `"serviceavailable":false`) {
		return &cosy.Error{Kind: cosy.ErrBusy, Status: status, Msg: firstLine(body)}
	}
	// 101「Signature invalid」：请求签名校验失败（机器指纹派生签名与上游会话态
	// 短暂失配）。这是 EnsureDT 重取会话可修复的临时态——refresh 修复，绝非
	// session dead。必须先于 Classify 的 403→SessionDead 兜底判定，否则连续两次
	// 签名抖动会把健康号永久禁用（需人工重登）。
	if strings.Contains(body, "Signature invalid") || strings.Contains(body, "signature invalid") {
		return &cosy.Error{Kind: cosy.ErrTokenExpired, Status: status, Msg: firstLine(body)}
	}
	return cosy.ClassifyError(status, firstLine(body))
}

// firstLine 取首行并限长（错误信息防刷屏）。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
