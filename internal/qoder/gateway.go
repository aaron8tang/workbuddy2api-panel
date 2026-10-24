// gateway.go Qoder 渠道网关：/v1/chat/completions 轮转调度 + 面板管理/登录端点。
// 池操作用 internal/cosy 通用池（PickExcluding + Get，无租约）。
// 特例：订阅缺失（qoder.com/pricing）绝不 Disable——那是套餐问题不是凭证问题。
package qoder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cosy"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// Config 网关配置。
type Config struct {
	Pool         *cosy.Pool
	Upstream     *Client
	AuthDir      string // qoder-<uid>.json 凭证目录
	MaxRotate    int
	HardCooldown time.Duration // 日额度耗尽 / 余额类
	SoftCooldown time.Duration // 429 / 404 / 无权益
	ErrThreshold int
	ErrCooldown  time.Duration
	DefaultModel string
}

// Gateway Qoder 渠道门面。
type Gateway struct {
	cfg Config

	loginMu sync.Mutex
	logins  map[string]*LoginTransaction

	mu             sync.Mutex
	dynamicModels  []modelEntry
	dynamicFetched time.Time
}

// New 构建网关并加载 AuthDir 凭证入池。
func New(cfg Config) *Gateway {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 8
	}
	if cfg.HardCooldown <= 0 {
		cfg.HardCooldown = 12 * time.Hour
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
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = "Qwen3.7-Max"
	}
	if cfg.Upstream == nil {
		cfg.Upstream = NewClient()
	}
	g := &Gateway{cfg: cfg, logins: map[string]*LoginTransaction{}}
	g.ReloadAccounts()
	return g
}

// Pool 暴露账号池。
func (g *Gateway) Pool() *cosy.Pool { return g.cfg.Pool }

// OAuthConfig 面板登录用 OAuth 参数。
func (g *Gateway) OAuthConfig() OAuthConfig { return g.cfg.Upstream.OAuth }

// ReloadAccounts 扫描 AuthDir 并与池对齐（状态保留在 state.json）。
func (g *Gateway) ReloadAccounts() {
	if g.cfg.AuthDir == "" || g.cfg.Pool == nil {
		return
	}
	creds, err := LoadDir(g.cfg.AuthDir)
	if err != nil {
		return
	}
	accts := make([]*cosy.Account, 0, len(creds))
	for _, cr := range creds {
		cr.completeFingerprint()
		accts = append(accts, cosy.NewAccount(cr.UID, cr.Name, cr.fingerprint(), cr))
	}
	g.cfg.Pool.SyncToDir(accts)
}

// fingerprint Qoder 凭证绑定登录时的 MachineID（上游拒绝其他身份的签名）。
// MachineToken 对齐 API-Console 的 applyFingerprint：无独立 token 时用 MachineID。
func (c *Cred) fingerprint() cosy.Fingerprint {
	return cosy.Fingerprint{MachineID: c.MachineID, MachineToken: c.MachineID, MachineType: "5"}
}

// completeFingerprint 无 MachineID 的导入凭证补齐并标记需落盘。
func (c *Cred) completeFingerprint() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(c.MachineID) == "" {
		c.MachineID = cosy.UUID()
	}
}

// Servable 渠道是否可服务。
func (g *Gateway) Servable() bool {
	for _, st := range g.cfg.Pool.List() {
		if !st.Disabled && !st.Cooling {
			return true
		}
	}
	return false
}

// HasAccounts 池内是否有账号。
func (g *Gateway) HasAccounts() bool { return len(g.cfg.Pool.List()) > 0 }

// ---------------------------------------------------------------------------
// 模型目录
// ---------------------------------------------------------------------------

// defaultModelKey 无 model 参数时的上游 key（qoder2api PREFERRED_DEFAULT_KEY）。
const defaultModelKey = "qmodel_latest"

// staticModels 动态目录不可得时的静态兜底（catalog-v5 chat 场景，无 auto）。
var staticModels = []modelEntry{
	{Key: "qmodel_preview", DisplayName: "Qwen3.8-Max-Preview", IsVL: true, IsReasoning: true},
	{Key: "qmodel_latest", DisplayName: "Qwen3.7-Max", IsVL: true, IsReasoning: true},
	{Key: "qmodel", DisplayName: "Qwen3.7-Plus", IsVL: true, IsReasoning: true},
	{Key: "q36fmodel", DisplayName: "Qwen3.6-Flash", IsVL: true},
	{Key: "dmodel", DisplayName: "DeepSeek-V4-Pro", IsVL: true, IsReasoning: true},
	{Key: "dfmodel", DisplayName: "DeepSeek-V4-Flash", IsVL: true},
	{Key: "gm51model", DisplayName: "GLM-5.2", IsVL: true, IsReasoning: true},
	{Key: "kmodel", DisplayName: "Kimi-K2.7-Code", IsVL: true},
	{Key: "mmodel", DisplayName: "MiniMax-M2.7"},
}

func (g *Gateway) fetchDynamic() []modelEntry {
	g.mu.Lock()
	if g.dynamicModels != nil && time.Since(g.dynamicFetched) < time.Hour {
		out := g.dynamicModels
		g.mu.Unlock()
		return out
	}
	g.mu.Unlock()

	var out []modelEntry
	for _, st := range g.cfg.Pool.List() {
		if st.Disabled || st.Cooling {
			continue
		}
		acct := g.cfg.Pool.Get(st.UID)
		if acct == nil {
			continue
		}
		cr, ok := acct.Data.(*Cred)
		if !ok || cr == nil {
			continue
		}
		if err := cr.EnsureDT(g.cfg.Upstream.OAuth, newHTTPClient()); err != nil {
			continue
		}
		models, err := g.cfg.Upstream.FetchModels(acct, cr)
		if err != nil {
			continue
		}
		out = models
		break
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if len(out) > 0 {
		g.dynamicModels = out
		g.dynamicFetched = time.Now()
		return out
	}
	if g.dynamicModels != nil && time.Since(g.dynamicFetched) < 5*time.Minute {
		return g.dynamicModels
	}
	g.dynamicFetched = time.Now()
	return nil
}

// ModelEntries 面板/客户端模型清单（id 带 "qoder:" 前缀；客户端名 = display 名）。
func (g *Gateway) ModelEntries() []map[string]any {
	infos := g.fetchDynamic()
	if len(infos) == 0 {
		infos = staticModels
	}
	out := make([]map[string]any, 0, len(infos))
	for _, m := range infos {
		name := m.DisplayName
		if name == "" {
			name = m.Key
		}
		e := map[string]any{
			"id":           "qoder:" + name,
			"object":       "model",
			"created":      1753600000,
			"owned_by":     "qoder",
			"upstream_key": m.Key,
			"reasoning":    m.IsReasoning,
		}
		if m.MaxInputTokens > 0 {
			e["context_length"] = m.MaxInputTokens
		}
		if m.PriceFactor != nil && *m.PriceFactor > 0 {
			e["price_factor"] = *m.PriceFactor
		}
		if m.IsVL {
			e["vision"] = true
		}
		out = append(out, e)
	}
	return out
}

// resolveModelEntry 把客户端模型名解析为上游条目。
// 接受 display 名、规范化名或裸上游 key；空/"auto" 走默认模型。
func (g *Gateway) resolveModelEntry(model string) (modelEntry, bool) {
	model = strings.TrimSpace(model)
	if model == "" || strings.EqualFold(model, "auto") {
		model = g.cfg.DefaultModel
	}
	lookup := func(name string) (modelEntry, bool) {
		for _, m := range g.fetchDynamic() {
			display := m.DisplayName
			if display == "" {
				display = m.Key
			}
			if display == name || normalizeModelName(display) == name || m.Key == name {
				return m, true
			}
		}
		for _, m := range staticModels {
			display := m.DisplayName
			if display == "" {
				display = m.Key
			}
			if display == name || normalizeModelName(display) == name || m.Key == name {
				return m, true
			}
		}
		return modelEntry{}, false
	}
	if m, ok := lookup(model); ok {
		return m, true
	}
	return lookup(normalizeModelName(model))
}

// normalizeModelName 客户端模型名规范化（小写、空格/下划线转连字符、保留点号）。
func normalizeModelName(s string) string {
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

// ---------------------------------------------------------------------------
// 对话入口
// ---------------------------------------------------------------------------

// ChatCompletions 处理一次 Qoder 渠道对话（已由主路由剥 "qoder:" 前缀）。
// onOutcome：SSE 已 200 的流内业务错误经它回传面板 outcome（reqlog.OutcomeReporter）。
func (g *Gateway) ChatCompletions(w http.ResponseWriter, r *http.Request, bareModel string,
	onAccount func(uid, nick string), onOutcome reqlog.OutcomeReporter) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if len(body) > maxBodyBytes {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 8MB limit")
		return
	}
	var req struct {
		Model             string           `json:"model"`
		Stream            bool             `json:"stream"`
		Messages          []map[string]any `json:"messages"`
		Tools             []any            `json:"tools"`
		ToolChoice        any              `json:"tool_choice"`
		ParallelToolCalls *bool            `json:"parallel_tool_calls"`
		Temperature       *float64         `json:"temperature"`
		TopP              *float64         `json:"top_p"`
		MaxTokens         int              `json:"max_tokens"`
		Stop              []string         `json:"stop"`
		ResponseFormat    map[string]any   `json:"response_format"`
		ReasoningEffort   string           `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse body: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "messages is required")
		return
	}

	// 模型解析：调用方显式裸模型优先；组条目 "|档位" 后缀拆思考档位。
	effort := req.ReasoningEffort
	model := bareModel
	if model == "" {
		model = req.Model
	}
	if i := strings.LastIndex(model, "|"); i >= 0 {
		if e := strings.TrimSpace(model[i+1:]); e != "" {
			effort = e
		}
		model = model[:i]
	}
	entry, ok := g.resolveModelEntry(model)
	if !ok {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_model",
			fmt.Sprintf("unknown model %q (see /v1/models)", model))
		return
	}

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < g.cfg.MaxRotate; i++ {
		acct := g.cfg.Pool.PickExcluding(tried)
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		cr, ok := acct.Data.(*Cred)
		if !ok || cr == nil {
			continue
		}
		if onAccount != nil {
			onAccount(acct.UID, acct.Nickname)
		}

		if err := cr.EnsureDT(g.cfg.Upstream.OAuth, newHTTPClient()); err != nil {
			lastErr = err
			if errors.Is(err, ErrReLoginRequired) {
				g.cfg.Pool.Disable(acct.UID, "re-login required")
			} else {
				g.cfg.Pool.Cooldown(acct.UID, cosy.CoolErr, g.cfg.ErrCooldown, "dt ensure: "+err.Error())
			}
			continue
		}

		opts := ChatOptions{
			Messages:          req.Messages,
			Tools:             req.Tools,
			ToolChoice:        req.ToolChoice,
			ParallelToolCalls: req.ParallelToolCalls,
			Temperature:       req.Temperature,
			TopP:              req.TopP,
			MaxTokens:         req.MaxTokens,
			Stop:              req.Stop,
			ResponseFormat:    req.ResponseFormat,
			ReasoningEffort:   effort,
			Model:             entry,
		}
		rc, err := g.cfg.Upstream.ChatForward(r.Context(), acct, cr, opts)
		if err != nil {
			lastErr = err
			g.noteChatError(acct.UID, err)
			continue
		}
		turnStarted := time.Now()
		// finishReport 一跳成功消费后上报 ask/finish（fire-and-forget，失败无害：
		// 服务端 TTL 兜底释放 lease）。排队协议闭环的一部分。
		finishReport := func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if ferr := g.cfg.Upstream.FinishRequest(ctx, acct, cr, opts, time.Since(turnStarted).Nanoseconds()); ferr != nil {
				log.Printf("qoder_chat uid=%s model=%s: queue finish: %v (server TTL will release the lease)", acct.UID, entry.Key, ferr)
			}
		}

		scan := &markerScanner{src: rc}
		if req.Stream {
			// 首包门（同 qoderwork）：第一个内容帧之前不写响应头。上游把容量/路由
			// 类拒绝放在 200 的流内错误帧里，门内命中即冷却换号——否则只能把 200
			// 连同错误帧透传，白白扔掉换号机会。
			verdict, pre, rest, ie, herr := cosy.PrefetchNestedSSE(scan)
			if verdict != cosy.HeadAnswered {
				scan.Close()
				if herr != nil {
					lastErr = herr
					g.noteChatError(acct.UID, herr)
					continue
				}
				if ie != nil {
					lastErr = ie
					g.noteChatError(acct.UID, ie)
					reportInStreamOutcome(onOutcome, acct.UID, acct.Nickname, ie)
					continue
				}
				lastErr = fmt.Errorf("upstream stream ended with no content")
				g.noteChatError(acct.UID, lastErr)
				continue
			}
			writeSSEHeaders(w)
			fl, _ := w.(http.Flusher)
			flush := func() {
				if fl != nil {
					fl.Flush()
				}
			}
			serr := cosy.StreamAsOpenAIPrefixed(w, pre, rest, entry.DisplayName, flush)
			scan.Close()
			if serr != nil {
				if scan.Hit() {
					// 200 SSE 内裹业务 403（订阅缺失）——冷却而非禁用
					log.Printf("qoder_chat uid=%s model=%s: in-stream entitlement refusal", acct.UID, entry.Key)
					g.cfg.Pool.Cooldown(acct.UID, cosy.CoolSoft, g.cfg.SoftCooldown, "no entitlement (in-stream)")
					reportOutcome(onOutcome, acct.UID, acct.Nickname, reqlog.OutcomeStreamError, http.StatusForbidden)
					return
				}
				log.Printf("qoder_chat uid=%s model=%s: stream error: %v", acct.UID, entry.Key, serr)
				g.noteChatError(acct.UID, serr)
				reportInStreamOutcome(onOutcome, acct.UID, acct.Nickname, serr)
			} else if scan.Hit() {
				// 200 SSE 内裹业务 403（订阅缺失）——冷却而非禁用
				log.Printf("qoder_chat uid=%s model=%s: in-stream entitlement refusal", acct.UID, entry.Key)
				g.cfg.Pool.Cooldown(acct.UID, cosy.CoolSoft, g.cfg.SoftCooldown, "no entitlement (in-stream)")
			} else {
				g.cfg.Pool.NoteSuccess(acct.UID)
				go finishReport()
			}
			return
		}
		resp, aerr := cosy.AggregateNested(scan, entry.DisplayName)
		scan.Close()
		if aerr != nil {
			if scan.Hit() {
				g.cfg.Pool.Cooldown(acct.UID, cosy.CoolSoft, g.cfg.SoftCooldown, "no entitlement (in-stream)")
				lastErr = ErrNoEntitlement
				continue
			}
			log.Printf("qoder_chat uid=%s model=%s: aggregate error: %v", acct.UID, entry.Key, aerr)
			g.noteChatError(acct.UID, aerr)
			lastErr = aerr
			continue
		}
		go finishReport()
		g.cfg.Pool.NoteSuccess(acct.UID)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	msg := "all qoder accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

// reportOutcome 面板 outcome 上报的 nil 安全包装。
func reportOutcome(onOutcome reqlog.OutcomeReporter, uid, nick, outcome string, status int) {
	if onOutcome != nil {
		onOutcome(uid, nick, outcome, status)
	}
}

// reportInStreamOutcome 流内业务错误的 outcome 映射：Busy 类（容量瞬时拒绝，
// 已换号/短冷却）语义上是上游过载 → 502 stream_error；其他（4028 拒收、未知码）
// → 502 stream_error。SSE 已写 200 头，状态码仅作面板展示。
func reportInStreamOutcome(onOutcome reqlog.OutcomeReporter, uid, nick string, err error) {
	var ie *cosy.InStreamError
	if !errors.As(err, &ie) {
		return
	}
	reportOutcome(onOutcome, uid, nick, reqlog.OutcomeStreamError, http.StatusBadGateway)
}

// noteChatError 按错误类别驱动池状态机。
func (g *Gateway) noteChatError(uid string, err error) {
	var ie *cosy.InStreamError
	if errors.As(err, &ie) {
		if ie.Busy() {
			// 容量类瞬时拒绝（10605 RESOURCE_LIMIT / provider_error）：账号无恙，
			// 但队列满/上游临时故障——立刻再选它还会撞墙。按帧内 retryAfterSeconds
			// （缺省 30s）短软冷却，让本轮轮转自然换号。
			d := ie.RetryAfter()
			if d <= 0 {
				d = g.cfg.SoftCooldown
			}
			if d < 10*time.Second {
				d = 10 * time.Second
			}
			g.cfg.Pool.Cooldown(uid, cosy.CoolSoft, d, "upstream busy (in-stream)")
			return
		}
		// 其他流内业务错误（4028/未知码）：计入连续失败，由熔断器统一退避
		g.cfg.Pool.NoteError(uid, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
		return
	}
	var ue *cosy.Error
	if !errors.As(err, &ue) {
		g.cfg.Pool.NoteError(uid, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
		return
	}
	switch {
	case errors.Is(err, ErrNoEntitlement):
		// 订阅缺失：禁用账号于事无补，软冷却换号即可
		g.cfg.Pool.Cooldown(uid, cosy.CoolSoft, g.cfg.SoftCooldown, "no entitlement")
	case errors.Is(err, ErrDailyCountExceeded):
		g.cfg.Pool.Cooldown(uid, cosy.CoolHard, g.cfg.HardCooldown, "daily count exceeded")
	default:
		switch ue.Kind {
		case cosy.ErrHardCredit:
			g.cfg.Pool.Cooldown(uid, cosy.CoolHard, g.cfg.HardCooldown, "余额不足")
		case cosy.ErrSoftRate:
			g.cfg.Pool.Cooldown(uid, cosy.CoolSoft, g.cfg.SoftCooldown, "429 rate limit")
		case cosy.ErrBusy:
			// 共享队列拒答：立刻换号重试，不冷却不刷错误计数
		case cosy.ErrTokenExpired:
			if acct := g.cfg.Pool.Get(uid); acct != nil {
				if cr, ok := acct.Data.(*Cred); ok && cr != nil {
					cr.mu.Lock()
					cr.DTExpiry = time.Time{}
					cr.mu.Unlock()
				}
			}
			g.cfg.Pool.NoteError(uid, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
		case cosy.ErrSessionDead:
			g.cfg.Pool.Disable(uid, "session dead")
		case cosy.ErrNotFound:
			g.cfg.Pool.Cooldown(uid, cosy.CoolSoft, g.cfg.SoftCooldown, "upstream 404")
		default:
			g.cfg.Pool.NoteError(uid, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
		}
	}
}

// markerScanner 流式读穿透扫描器：捕获 200 SSE 内包裹的业务错误标记
//（订阅缺失/日额度），供流结束后处置账号。
type markerScanner struct {
	src        io.ReadCloser
	hitPricing bool
	hitDaily   bool
}

func (s *markerScanner) Read(p []byte) (int, error) {
	n, err := s.src.Read(p)
	if n > 0 {
		chunk := string(p[:n])
		if strings.Contains(chunk, "qoder.com/pricing") {
			s.hitPricing = true
		}
		if strings.Contains(strings.ToLower(chunk), "billing daily count exceeded") {
			s.hitDaily = true
		}
	}
	return n, err
}

// Hit 是否捕获到包内业务错误标记。
func (s *markerScanner) Hit() bool { return s.hitPricing || s.hitDaily }

func (s *markerScanner) Close() error { return s.src.Close() }

// ---------------------------------------------------------------------------
// 面板管理端点
// ---------------------------------------------------------------------------

// AccountSummary 单账号面板视图。
type AccountSummary struct {
	cosy.Status
	DTExpiresAt  int64    `json:"dt_expires_at,omitempty"`
	DRTExpiresAt int64    `json:"drt_expires_at,omitempty"`
	HasDRT       bool     `json:"has_drt"`
	Email        string   `json:"email,omitempty"`
	OrgID        string   `json:"org_id,omitempty"`
	OrgTags      []string `json:"org_tags,omitempty"`
}

func (g *Gateway) accountSummaries() []AccountSummary {
	list := g.cfg.Pool.List()
	out := make([]AccountSummary, 0, len(list))
	for _, st := range list {
		s := AccountSummary{Status: st}
		if acct := g.cfg.Pool.Get(st.UID); acct != nil {
			if cr, ok := acct.Data.(*Cred); ok && cr != nil {
				cr.mu.Lock()
				if !cr.DTExpiry.IsZero() {
					s.DTExpiresAt = cr.DTExpiry.Unix()
				}
				if !cr.DRTExpiry.IsZero() {
					s.DRTExpiresAt = cr.DRTExpiry.Unix()
				}
				s.HasDRT = strings.TrimSpace(cr.DRT) != ""
				s.Email = cr.Email
				s.OrgID = cr.OrgID
				s.OrgTags = cr.OrgTags
				cr.mu.Unlock()
			}
		}
		out = append(out, s)
	}
	return out
}

// AccountsHandler GET /panel/api/qoder/accounts
func (g *Gateway) AccountsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"accounts": g.accountSummaries()})
}

// ImportHandler POST /panel/api/qoder/accounts/import
func (g *Gateway) ImportHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JSON string `json:"json"`
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err := json.Unmarshal(raw, &req); err == nil && strings.TrimSpace(req.JSON) != "" {
		raw = []byte(req.JSON)
	}
	cr, err := ParseCred(raw)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_credential", err.Error())
		return
	}
	cr.completeFingerprint()
	_, _, derr := cr.EnsureRuntime()
	if derr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "runtime_derive_failed", derr.Error())
		return
	}
	if g.cfg.AuthDir != "" {
		if err := cr.Save(g.cfg.AuthDir); err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "save_failed", err.Error())
			return
		}
	}
	g.cfg.Pool.Add(cosy.NewAccount(cr.UID, cr.Name, cr.fingerprint(), cr))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": cr.UID})
}

// DeleteHandler DELETE /panel/api/qoder/accounts/{uid}
func (g *Gateway) DeleteHandler(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing uid")
		return
	}
	if g.cfg.AuthDir != "" {
		_ = os.Remove(filepath.Join(g.cfg.AuthDir, "qoder-"+uid+".json"))
	}
	g.cfg.Pool.Remove(uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// PatchHandler PATCH /panel/api/qoder/accounts/{uid}
func (g *Gateway) PatchHandler(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse body: "+err.Error())
		return
	}
	switch req.Action {
	case "enable":
		g.cfg.Pool.Enable(uid)
	case "disable":
		g.cfg.Pool.Disable(uid, "manual disable")
	case "clear_cooldown":
		g.cfg.Pool.ClearCooldown(uid)
	default:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "unknown action "+req.Action)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// RefreshHandler POST /panel/api/qoder/accounts/{uid}/refresh —— 刷 token + 身份落盘。
func (g *Gateway) RefreshHandler(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	acct := g.cfg.Pool.Get(uid)
	if acct == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "unknown account "+uid)
		return
	}
	cr, ok := acct.Data.(*Cred)
	if !ok || cr == nil {
		writeOpenAIError(w, http.StatusConflict, "no_credential", "account has no credential loaded")
		return
	}
	if err := cr.EnsureDT(g.cfg.Upstream.OAuth, newHTTPClient()); err != nil {
		if errors.Is(err, ErrReLoginRequired) {
			g.cfg.Pool.Disable(uid, "re-login required")
			writeOpenAIError(w, http.StatusConflict, "relogin_required", err.Error())
		} else {
			writeOpenAIError(w, http.StatusBadGateway, "refresh_failed", err.Error())
		}
		return
	}
	// 身份可选刷新（userinfo 故障不否定有效凭证）
	if p, err := FetchProfile(g.cfg.Upstream.OAuth, newHTTPClient(), cr.DT); err == nil {
		p.Apply(cr)
	}
	cr.InvalidateRuntime()
	if _, _, err := cr.EnsureRuntime(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "runtime_derive_failed", err.Error())
		return
	}
	if g.cfg.AuthDir != "" {
		_ = cr.Save(g.cfg.AuthDir)
	}
	acct.Nickname = cr.Name
	g.cfg.Pool.SaveState()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid})
}

// JSONHandler GET /panel/api/qoder/accounts/{uid}/json —— 原始凭证 JSON。
func (g *Gateway) JSONHandler(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if g.cfg.AuthDir != "" {
		raw, err := os.ReadFile(filepath.Join(g.cfg.AuthDir, "qoder-"+uid+".json"))
		if err == nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(raw)
			return
		}
	}
	if acct := g.cfg.Pool.Get(uid); acct != nil {
		if cr, ok := acct.Data.(*Cred); ok && cr != nil {
			writeJSON(w, http.StatusOK, cr)
			return
		}
	}
	writeOpenAIError(w, http.StatusNotFound, "not_found", "unknown account "+uid)
}

// ModelsRefreshHandler POST /panel/api/qoder/models/refresh —— 强制刷新动态目录。
func (g *Gateway) ModelsRefreshHandler(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.dynamicModels = nil
	g.dynamicFetched = time.Time{}
	g.mu.Unlock()
	models := g.fetchDynamic()
	if len(models) == 0 {
		writeOpenAIError(w, http.StatusBadGateway, "fetch_failed", "no dynamic catalog available (static fallback in effect)")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(models)})
}

// ---------------------------------------------------------------------------
// 面板设备流登录
// ---------------------------------------------------------------------------

// LoginStartHandler POST /panel/api/qoder/login —— 发起设备授权。
func (g *Gateway) LoginStartHandler(w http.ResponseWriter, r *http.Request) {
	tx, err := StartLogin(g.cfg.Upstream.OAuth)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "login_start_failed", err.Error())
		return
	}
	g.loginMu.Lock()
	g.logins[tx.Nonce] = tx
	g.loginMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"verify_url": tx.VerifyURL,
		"nonce":      tx.Nonce,
		"expires_at": tx.ExpiresAt.Unix(),
	})
}

// LoginResultHandler GET /panel/api/qoder/login/result?nonce=... —— 轮询授权结果。
func (g *Gateway) LoginResultHandler(w http.ResponseWriter, r *http.Request) {
	nonce := r.URL.Query().Get("nonce")
	g.loginMu.Lock()
	tx := g.logins[nonce]
	g.loginMu.Unlock()
	if tx == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "unknown or expired login transaction")
		return
	}
	if time.Now().After(tx.ExpiresAt) {
		g.loginMu.Lock()
		delete(g.logins, nonce)
		g.loginMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"status": "expired"})
		return
	}
	cr, err := PollLogin(g.cfg.Upstream.OAuth, tx, newHTTPClient())
	if err != nil {
		if errors.Is(err, ErrAuthPending) {
			writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	g.loginMu.Lock()
	delete(g.logins, nonce)
	g.loginMu.Unlock()

	// 补身份元数据（失败不阻塞导入）
	if p, ferr := FetchProfile(g.cfg.Upstream.OAuth, newHTTPClient(), cr.DT); ferr == nil {
		p.Apply(cr)
	}
	cr.completeFingerprint()
	if _, _, derr := cr.EnsureRuntime(); derr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "error": "runtime derive: " + derr.Error()})
		return
	}
	if g.cfg.AuthDir != "" {
		if serr := cr.Save(g.cfg.AuthDir); serr != nil {
			log.Printf("qoder login uid=%s: persist failed: %v", cr.UID, serr)
		}
	}
	g.cfg.Pool.Add(cosy.NewAccount(cr.UID, cr.Name, cr.fingerprint(), cr))
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "uid": cr.UID, "name": cr.Name})
}

// LoginCancelHandler POST /panel/api/qoder/login/cancel —— 取消授权事务。
func (g *Gateway) LoginCancelHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Nonce string `json:"nonce"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req)
	g.loginMu.Lock()
	delete(g.logins, req.Nonce)
	g.loginMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// HTTP 小工具
// ---------------------------------------------------------------------------

// maxBodyBytes 请求体上限（对齐主链路 8MB）。
const maxBodyBytes = 8 << 20

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
		"error": map[string]any{
			"message": msg,
			"type":    code,
			"code":    code,
		},
	})
}
