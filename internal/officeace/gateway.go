// gateway.go OfficeAce 渠道门面：ChatCompletions（恒流式 → 按需聚合）+ 模型目录
// 缓存（Basic 链）+ 控制面自动续期（refresh token 一次性轮换）+ 面板管理端点。
package officeace

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/stagger"
)

// Config 网关配置。
type Config struct {
	AuthDir      string // officeace-*.json 凭证目录
	MaxRotate    int    // 0 = 3
	SoftCooldown time.Duration
	ErrThreshold int
	ErrCooldown  time.Duration
}

// Gateway 门面。
type Gateway struct {
	cfg      Config
	upstream *Upstream
	authDir  string

	mu      sync.Mutex
	creds   []*Credential
	cooled  map[string]time.Time
	errCnt  map[string]int

	loginMu sync.Mutex
	logins  map[string]*LoginFlow

	// checkinMu 签到批量互斥：面板手动端点与 scheduler 定时钩子可能同时触发
	// CheckinAll，忙时 TryLock 失败返回空（2026-10-23 事故整改第二阶段）。
	checkinMu sync.Mutex
}

// New 构建网关并加载凭证目录。
func New(cfg Config) *Gateway {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 2 * time.Minute
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 5 * time.Minute
	}
	g := &Gateway{
		cfg: cfg, upstream: NewUpstream(), authDir: cfg.AuthDir,
		cooled: map[string]time.Time{}, errCnt: map[string]int{},
		logins: map[string]*LoginFlow{},
	}
	g.ReloadAccounts()
	return g
}

// ReloadAccounts 扫描目录对齐。
func (g *Gateway) ReloadAccounts() {
	if g.authDir == "" {
		return
	}
	creds, err := LoadDir(g.authDir)
	if err != nil {
		return
	}
	g.mu.Lock()
	g.creds = creds
	g.mu.Unlock()
}

// List 全部凭据快照。
func (g *Gateway) List() []*Credential {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]*Credential, len(g.creds))
	copy(out, g.creds)
	return out
}

// Get 按 uid 取。
func (g *Gateway) Get(uid string) *Credential {
	for _, c := range g.List() {
		if c.SafeUID() == uid {
			return c
		}
	}
	return nil
}

// Servable 至少一个可转发账号且不在冷却。
func (g *Gateway) Servable() bool {
	now := time.Now()
	for _, c := range g.List() {
		if !c.CanForward() {
			continue
		}
		g.mu.Lock()
		until := g.cooled[c.SafeUID()]
		g.mu.Unlock()
		if until.IsZero() || now.After(until) {
			return true
		}
	}
	return false
}

// HasAccounts 是否有可转发账号。
func (g *Gateway) HasAccounts() bool {
	for _, c := range g.List() {
		if c.CanForward() {
			return true
		}
	}
	return false
}

// upsert 写回并持久化。
func (g *Gateway) upsert(cred *Credential) {
	g.mu.Lock()
	found := false
	for i, c := range g.creds {
		if c.SafeUID() == cred.SafeUID() {
			g.creds[i] = cred
			found = true
			break
		}
	}
	if !found {
		g.creds = append(g.creds, cred)
	}
	g.mu.Unlock()
	if g.authDir != "" {
		_ = cred.SaveAtomic(g.authDir)
	}
}

// RefreshControlPlane 用一次性 refresh token + DPoP 私钥续期控制面并写回。
func (g *Gateway) RefreshControlPlane(cred *Credential) error {
	cred.mu.Lock()
	refreshToken := cred.RefreshToken
	pair := cred.DpopKeyPair
	cred.mu.Unlock()
	if strings.TrimSpace(refreshToken) == "" {
		return fmt.Errorf("OfficeAce 账号没有 refresh token，无法续期：请重新登录一次")
	}
	ak, sk, st, pid, expiresAt, userName, nextRefresh, err := RefreshLogin(pair, refreshToken)
	if err != nil {
		return fmt.Errorf("OfficeAce 续期失败: %w", err)
	}
	cred.UpdateControlPlane(ak, sk, st, pid, expiresAt, nextRefresh)
	// 名字自愈：登录那刻可能没拿到名字，每续一次问一次。
	if userName != "" {
		cred.mu.Lock()
		cred.Nickname = userName
		cred.mu.Unlock()
	}
	g.upsert(cred)
	log.Printf("[officeace] 控制面凭据已续期: %s", cred.SafeUID())
	return nil
}

// ensureControlPlane 临期（30 分钟）自动续。
func (g *Gateway) ensureControlPlane(cred *Credential) *Credential {
	if !cred.HasControlPlane() || !cred.ControlPlaneExpiring() {
		return cred
	}
	if err := g.RefreshControlPlane(cred); err != nil {
		log.Printf("[officeace] 自动续期失败（%v）；转发不受影响（网关 Basic 不过期）", err)
	}
	return cred
}

// ---------------------------------------------------------------------------
// 模型清单（Basic 链 + 倍率 + probe 隐藏）
// ---------------------------------------------------------------------------

// ModelEntries /v1/models 与面板模型清单（id 带 "officeace:" 前缀；probe 隐藏的
// 模型不出现——门禁：藏掉的客户端看不到也点不动）。缓存为空（重启后首次访问）
// 时同步拉一次目录——目录是被动刷新的，不补拉的话重启后第一波请求看不到
// officeace 模型（TTL 内只拉一次，并发重复拉取无害）。
func (g *Gateway) ModelEntries() []map[string]any {
	entries := CachedModels()
	if len(entries) == 0 {
		if _, err := g.refreshCatalog(false); err != nil {
			return nil
		}
		entries = CachedModels()
		if len(entries) == 0 {
			return nil
		}
	}
	entries = AttachRates(entries, lastRates)
	out := make([]map[string]any, 0, len(entries))
	for _, m := range entries {
		if IsHidden(m.ID) {
			continue
		}
		e := map[string]any{
			"id": "officeace:" + m.ID, "object": "model",
			"created": 1753600000, "owned_by": "officeace",
		}
		if m.DisplayName != "" {
			e["display_name"] = m.DisplayName
		}
		if m.ContextLength > 0 {
			e["context_length"] = m.ContextLength
		}
		if m.Credits != "" {
			e["credits"] = m.Credits
		}
		out = append(out, e)
	}
	return out
}

// refreshCatalog 拉目录 + 倍率 + 触发探测。
func (g *Gateway) refreshCatalog(force bool) ([]CatalogEntry, error) {
	cred := g.pickAny()
	if cred == nil {
		return nil, fmt.Errorf("没有可用的 OfficeAce 账号")
	}
	entries, err := g.upstream.FetchModels(cred, force)
	if err != nil {
		return nil, err
	}
	// 倍率走控制面 V11 链（AK/SK 会过期），失败不阻断目录。
	if cp := g.ensureControlPlane(cred); cp.HasControlPlane() {
		if rates, rerr := g.upstream.FetchRates(cp); rerr == nil {
			entries = AttachRates(entries, rates)
		}
	}
	g.upstream.MaybeProbe(cred, entries, force)
	return entries, nil
}

func (g *Gateway) pickAny() *Credential {
	now := time.Now()
	for _, c := range g.List() {
		if !c.CanForward() {
			continue
		}
		g.mu.Lock()
		until := g.cooled[c.SafeUID()]
		g.mu.Unlock()
		if until.IsZero() || now.After(until) {
			return c
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 对话入口
// ---------------------------------------------------------------------------

// ChatCompletions 处理一次 OfficeAce 渠道对话（上游恒流式；非流式本地聚合）。
func (g *Gateway) ChatCompletions(w http.ResponseWriter, r *http.Request, bareModel string,
	onAccount func(uid, nick string), onOutcome reqlog.OutcomeReporter) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if len(body) > 8<<20 {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 8MB limit")
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)
	model := bareModel
	if model == "" || model == "default" {
		model = peek.Model
	}
	if strings.TrimSpace(model) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_model", "model is required")
		return
	}
	// 大小写无关解析到目录真名；隐藏（probe 判死）的模型当 model_not_found。
	entries := CachedModels()
	for _, e := range entries {
		if strings.EqualFold(e.ID, model) {
			model = e.ID
			break
		}
	}
	if IsHidden(model) {
		writeOpenAIError(w, http.StatusBadRequest, "model_not_found",
			fmt.Sprintf("model %q 在该账号上不可用（上游明确拒绝；见 /v1/models 可用清单）", model))
		return
	}

	tried := map[string]bool{}
	var lastErr string
	for i := 0; i < g.cfg.MaxRotate; i++ {
		var cred *Credential
		now := time.Now()
		for _, c := range g.List() {
			if !c.CanForward() || tried[c.SafeUID()] {
				continue
			}
			g.mu.Lock()
			until := g.cooled[c.SafeUID()]
			g.mu.Unlock()
			if until.IsZero() || now.After(until) {
				cred = c
				break
			}
		}
		if cred == nil {
			break
		}
		tried[cred.SafeUID()] = true
		if onAccount != nil {
			cred.mu.Lock()
			nick := cred.Nickname
			cred.mu.Unlock()
			onAccount(cred.SafeUID(), nick)
		}
		bmap := map[string]any{}
		if err := json.Unmarshal(body, &bmap); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求体不是 JSON 对象: "+err.Error())
			return
		}
		PrepareChatBody(bmap, model)
		payload, err := json.Marshal(bmap)
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "serialize_failed", err.Error())
			return
		}
		rc, err := g.upstream.ChatForward(cred, payload)
		if err != nil {
			lastErr = err.Error()
			if ue, ok := err.(*UpstreamError); ok {
				status, cooldownable := ue.Classify()
				if cooldownable {
					g.cooldown(cred.SafeUID(), g.cfg.SoftCooldown, ue.Error())
				} else {
					g.noteError(cred.SafeUID())
				}
				if onOutcome != nil {
					onOutcome(cred.SafeUID(), nicknameOf(cred), "stream_error", status)
				}
				if status == 413 || status == 401 || status == 403 {
					// Fatal 不换号（81004/81009 等换号也没用），直接透传。
					writeOpenAIError(w, status, upstreamCode(status, ue.Body), ue.Error())
					return
				}
			} else {
				g.noteError(cred.SafeUID())
			}
			continue
		}
		g.noteSuccess(cred.SafeUID())
		if peek.Stream {
			writeSSEHeaders(w)
			flusher, _ := w.(http.Flusher)
			buf := make([]byte, 8<<10)
			for {
				n, rerr := rc.Read(buf)
				if n > 0 {
					_, _ = w.Write(buf[:n])
					if flusher != nil {
						flusher.Flush()
					}
				}
				if rerr != nil {
					break
				}
			}
			rc.Close()
			return
		}
		out, fault := Aggregate(rc, model)
		if fault != nil {
			if onOutcome != nil {
				onOutcome(cred.SafeUID(), nicknameOf(cred), "stream_error", fault.Status)
			}
			if fault.Status == http.StatusForbidden { // content_filter 假答复
				writeOpenAIError(w, fault.Status, "content_filter", fault.Message)
				return
			}
			lastErr = fault.Message
			continue
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
		return
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
		"all officeace accounts unavailable (cooling/disabled)"+ifStr2(lastErr))
}

func nicknameOf(c *Credential) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Nickname
}

func ifStr2(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}

// upstreamCode 从上游体提业务码。
func upstreamCode(status int, body string) string {
	lowered := strings.ToLower(body)
	switch {
	case strings.Contains(lowered, "81004"), strings.Contains(lowered, "81009"):
		return "model_not_found"
	case status == 413:
		return "request_too_large"
	case status == 401:
		return "invalid_api_key"
	}
	return "upstream_error"
}

// Aggregate 非流式聚合（SSE → 一条 completion）。返回 (体, fault)。
// content_filter 假答复折成 403（可轮换失败）。
func Aggregate(rc io.ReadCloser, model string) ([]byte, *UpstreamFault) {
	defer rc.Close()
	var content, reasoning strings.Builder
	type toolCall struct {
		Index    int    `json:"index"`
		ID       string `json:"id,omitempty"`
		Type     string `json:"type,omitempty"`
		Function struct {
			Name      string `json:"name,omitempty"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	tools := map[int]*toolCall{}
	var toolOrder []int
	var usage map[string]any
	var finish string
	var chatID string
	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		line := scanner.Text()
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" || payload == "[DONE]" {
			continue
		}
		// 流内错误帧：带 error 字段 ⇒ 抛 502。
		var probe map[string]any
		if json.Unmarshal([]byte(payload), &probe) == nil {
			if e, has := probe["error"]; has {
				em, _ := e.(map[string]any)
				msg := "上游流式返回错误"
				if s, _ := em["message"].(string); s != "" {
					msg = s
				}
				return nil, &UpstreamFault{Status: 502, Message: msg}
			}
		}
		var frame struct {
			ID      string `json:"id"`
			Usage   map[string]any `json:"usage"`
			Choices []struct {
				Delta struct {
					Content   *string    `json:"content"`
					Reasoning *string    `json:"reasoning_content"`
					ToolCalls []toolCall `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &frame) != nil {
			continue
		}
		if frame.ID != "" && chatID == "" {
			chatID = frame.ID
		}
		if frame.Usage != nil {
			usage = frame.Usage
		}
		for _, ch := range frame.Choices {
			if ch.Delta.Content != nil {
				content.WriteString(*ch.Delta.Content)
			}
			if ch.Delta.Reasoning != nil {
				reasoning.WriteString(*ch.Delta.Reasoning)
			}
			for _, tc := range ch.Delta.ToolCalls {
				slot := tools[tc.Index]
				if slot == nil {
					slot = &toolCall{Index: tc.Index, ID: tc.ID, Type: tc.Type}
					slot.Function.Name = tc.Function.Name
					tools[tc.Index] = slot
					toolOrder = append(toolOrder, tc.Index)
				}
				if slot.ID == "" && tc.ID != "" {
					slot.ID = tc.ID
				}
				if slot.Function.Name == "" && tc.Function.Name != "" {
					slot.Function.Name = tc.Function.Name
				}
				slot.Function.Arguments += tc.Function.Arguments
			}
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
		}
	}
	if content.Len() == 0 && reasoning.Len() == 0 && len(toolOrder) == 0 {
		return nil, &UpstreamFault{Status: 502, Message: "OfficeAce 上游流结束但未返回任何内容"}
	}
	// 内容审核拒绝：HTTP 200 + 正常收尾 + content_filter ⇒ 可轮换失败。
	if finish == "content_filter" {
		return nil, &UpstreamFault{Status: 403, Message: "内容审核拒绝（content_filter）"}
	}
	if finish == "" {
		if len(toolOrder) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	if chatID == "" {
		chatID = "chatcmpl-officeace-" + randomHex(6)
	}
	msg := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	var toolList []any
	for _, idx := range toolOrder {
		if tc := tools[idx]; tc != nil {
			toolList = append(toolList, tc)
		}
	}
	if len(toolList) > 0 {
		msg["tool_calls"] = toolList
	}
	out := map[string]any{
		"id": chatID, "object": "chat.completion",
		"created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
	}
	if usage != nil {
		out["usage"] = usage
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, &UpstreamFault{Status: 502, Message: "聚合响应序列化失败: " + err.Error()}
	}
	return raw, nil
}

// UpstreamFault 非流式聚合的失败。
type UpstreamFault struct {
	Status  int
	Message string
}

// ---------------------------------------------------------------------------
// 池操作
// ---------------------------------------------------------------------------

func (g *Gateway) cooldown(uid string, d time.Duration, reason string) {
	g.mu.Lock()
	g.cooled[uid] = time.Now().Add(d)
	g.errCnt[uid] = 0
	g.mu.Unlock()
}

func (g *Gateway) noteError(uid string) {
	g.mu.Lock()
	g.errCnt[uid]++
	if g.cfg.ErrThreshold > 0 && g.errCnt[uid] >= g.cfg.ErrThreshold {
		g.cooled[uid] = time.Now().Add(g.cfg.ErrCooldown)
		g.errCnt[uid] = 0
	}
	g.mu.Unlock()
}

func (g *Gateway) noteSuccess(uid string) {
	g.mu.Lock()
	g.errCnt[uid] = 0
	g.mu.Unlock()
}

// StatusView 单账号面板视图。
type StatusView struct {
	UID           string `json:"uid"`
	Nickname      string `json:"nickname,omitempty"`
	BaseURL       string `json:"base_url,omitempty"`
	CanForward    bool   `json:"can_forward"`
	HasControl    bool   `json:"has_control_plane"`
	HasRefresh    bool   `json:"has_refresh"`
	ExpiresAtMs   int64  `json:"expires_at_ms,omitempty"`
	Cooling       bool   `json:"cooling"`
	Until         string `json:"until,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// AccountsHandler GET /panel/api/officeace/accounts
func (g *Gateway) AccountsHandler(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	out := make([]StatusView, 0)
	for _, c := range g.List() {
		uid := c.SafeUID()
		c.mu.Lock()
		view := StatusView{
			UID: uid, Nickname: c.Nickname, BaseURL: c.BaseURL,
			CanForward: c.CanForward(), HasControl: c.HasControlPlane(),
			HasRefresh: strings.TrimSpace(c.RefreshToken) != "",
			ExpiresAtMs: c.ExpiresAtMs,
		}
		c.mu.Unlock()
		g.mu.Lock()
		until := g.cooled[uid]
		g.mu.Unlock()
		if !until.IsZero() && now.Before(until) {
			view.Cooling = true
			view.Until = until.Format(time.RFC3339)
		}
		out = append(out, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// StatusHandler GET /panel/api/officeace/status
func (g *Gateway) StatusHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": true, "servable": g.Servable(), "has_accounts": g.HasAccounts(),
	})
}

// ImportHandler POST /panel/api/officeace/accounts/import
func (g *Gateway) ImportHandler(w http.ResponseWriter, r *http.Request) {
	raw, rerr := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if rerr != nil || len(raw) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing credential json")
		return
	}
	// 兼容 {json:"..."} 包裹形态（与 qoderwork ImportHandler 同口径）；凭证本体直接收。
	var wrapped struct {
		JSON string `json:"json"`
	}
	if json.Unmarshal(raw, &wrapped) == nil && strings.TrimSpace(wrapped.JSON) != "" {
		raw = []byte(wrapped.JSON)
	}
	cr, err := ParseCred(raw)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_credential", err.Error())
		return
	}
	g.upsert(cr)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": cr.SafeUID(), "name": cr.Nickname})
}

// DeleteHandler DELETE /panel/api/officeace/accounts/{uid}
func (g *Gateway) DeleteHandler(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	g.mu.Lock()
	kept := g.creds[:0]
	for _, c := range g.creds {
		if c.SafeUID() != uid {
			kept = append(kept, c)
		}
	}
	g.creds = kept
	g.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// PatchRequest 账号操作。
type PatchRequest struct {
	Action string `json:"action"` // enable | disable | clear_cooldown | refresh
}

// PatchHandler PATCH /panel/api/officeace/accounts/{uid}
func (g *Gateway) PatchHandler(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	var req PatchRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse body: "+err.Error())
		return
	}
	cred := g.Get(uid)
	if cred == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "unknown account "+uid)
		return
	}
	switch req.Action {
	case "enable", "clear_cooldown":
		g.mu.Lock()
		delete(g.cooled, uid)
		g.errCnt[uid] = 0
		g.mu.Unlock()
	case "disable":
		g.mu.Lock()
		g.cooled[uid] = time.Now().Add(365 * 24 * time.Hour)
		g.mu.Unlock()
	case "refresh":
		if err := g.RefreshControlPlane(cred); err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "refresh_failed", err.Error())
			return
		}
	default:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "unknown action "+req.Action)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// BalanceHandler POST /panel/api/officeace/accounts/{uid}/balance
func (g *Gateway) BalanceHandler(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	cred := g.Get(uid)
	if cred == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "unknown account "+uid)
		return
	}
	cp := g.ensureControlPlane(cred)
	if !cp.HasControlPlane() {
		writeOpenAIError(w, http.StatusBadRequest, "usage_not_configured",
			"OfficeAce 账号没有控制面凭据（导入时只给了网关 Basic 那一对）")
		return
	}
	sub, err := g.upstream.FetchSubscription(cp)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "subscription_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "subscription": sub})
}

// CheckinResult 单账号签到结果（纯数据形态，供面板端点与自动签到共用）。
type CheckinResult struct {
	UID string `json:"uid"`
	OK  bool   `json:"ok"`
	Msg string `json:"msg,omitempty"`
}

// CheckinAll 全部账号领奖励（签到）。纯函数形态——面板端点与 scheduler 自动签到
// 共用同一份逻辑（此前逻辑内联在 HTTP handler 里，scheduler 无法复用）。
func (g *Gateway) CheckinAll() []CheckinResult {
	if !g.checkinMu.TryLock() {
		log.Printf("[officeace] checkin: 批量签到执行中，本次触发跳过（闸门互斥）")
		return nil
	}
	defer g.checkinMu.Unlock()
	var out []CheckinResult
	first := true
	for _, cred := range g.List() {
		res := CheckinResult{UID: cred.SafeUID()}
		cp := g.ensureControlPlane(cred)
		if !cp.HasControlPlane() {
			res.Msg = "no control plane credential"
			out = append(out, res)
			continue
		}
		// 账号间随机错峰（2026-10-23 事故整改）：同渠道多账号绝不批量同刻打上游。
		if !first {
			time.Sleep(stagger.Heavy())
		}
		first = false
		before, err := g.upstream.FetchSubscription(cp)
		if err != nil {
			res.Msg = err.Error()
			out = append(out, res)
			continue
		}
		after, err := g.upstream.ClaimBonus(cp)
		if err != nil {
			res.Msg = err.Error()
			out = append(out, res)
			continue
		}
		gained := 0.0
		if before != nil && after != nil {
			bm := map[string]float64{}
			for _, b := range before.Credits {
				if b.Remain != nil {
					bm[b.Name] = *b.Remain
				}
			}
			for _, b := range after.Credits {
				if b.Remain == nil {
					continue
				}
				if prev, ok := bm[b.Name]; ok {
					if *b.Remain-prev > 0 {
						gained += *b.Remain - prev
					}
				} else {
					gained += *b.Remain
				}
			}
		}
		res.OK = true
		if gained > 0 {
			res.Msg = fmt.Sprintf("获得 %.0f 积分", gained)
		} else {
			res.Msg = "今日已领取或无可领奖励"
		}
		out = append(out, res)
	}
	return out
}

// CheckinHandler POST /panel/api/officeace/checkin —— 全部账号领奖励（签到）。
func (g *Gateway) CheckinHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"results": g.CheckinAll()})
}

// ModelsRefreshHandler POST /panel/api/officeace/models/refresh
func (g *Gateway) ModelsRefreshHandler(w http.ResponseWriter, r *http.Request) {
	entries, err := g.refreshCatalog(true)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "fetch_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(entries)})
}

// ---------------------------------------------------------------------------
// 登录（云端轮询取码；面板 2.5s 轮询状态）
// ---------------------------------------------------------------------------

// LoginStartHandler POST /panel/api/officeace/login
func (g *Gateway) LoginStartHandler(w http.ResponseWriter, r *http.Request) {
	flow, err := StartLogin()
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "login_start_failed", err.Error())
		return
	}
	nonce := randomHex(8)
	g.loginMu.Lock()
	g.logins[nonce] = flow
	g.loginMu.Unlock()
	time.AfterFunc(5*time.Minute, func() {
		g.loginMu.Lock()
		delete(g.logins, nonce)
		g.loginMu.Unlock()
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"verify_url": flow.AuthURL, "nonce": nonce,
		"expires_at": time.Now().Add(5 * time.Minute).Unix(),
	})
}

// LoginResultHandler GET /panel/api/officeace/login/result?nonce=
// 登录是**服务端中介轮询**：这里替用户轮询云端 auth/code，拿到码就换凭据落盘。
func (g *Gateway) LoginResultHandler(w http.ResponseWriter, r *http.Request) {
	nonce := r.URL.Query().Get("nonce")
	g.loginMu.Lock()
	flow := g.logins[nonce]
	g.loginMu.Unlock()
	if flow == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"status": "expired"})
		return
	}
	code, err := PollCode(flow.State)
	if err != nil {
		if err == ErrAuthPending {
			writeJSON(w, http.StatusAccepted, map[string]any{"status": "pending"})
			return
		}
		g.loginMu.Lock()
		delete(g.logins, nonce)
		g.loginMu.Unlock()
		writeJSON(w, http.StatusBadGateway, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	cred, err := ExchangeCode(flow, code)
	if err != nil {
		g.loginMu.Lock()
		delete(g.logins, nonce)
		g.loginMu.Unlock()
		writeJSON(w, http.StatusBadGateway, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	g.loginMu.Lock()
	delete(g.logins, nonce)
	g.loginMu.Unlock()
	g.upsert(cred)
	log.Printf("[officeace] account added via web-login: uid=%s name=%s", cred.SafeUID(), cred.Nickname)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "uid": cred.SafeUID(), "name": cred.Nickname})
}

// LoginCancelHandler POST /panel/api/officeace/login/cancel
func (g *Gateway) LoginCancelHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Nonce string `json:"nonce"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req)
	g.loginMu.Lock()
	delete(g.logins, req.Nonce)
	g.loginMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// HTTP 小工具
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": code, "code": code},
	})
}
