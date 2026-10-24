// gateway.go 千问办公渠道网关：/v1/chat/completions 轮转调度 + 面板管理端点。
// 凭证来源为桌面端导入（无浏览器设备流——登录在千问办公客户端内完成）。
package qwenwork

import (
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
	AuthDir      string // qwenwork-<uid>.json 凭证目录
	MaxRotate    int
	HardCooldown time.Duration
	SoftCooldown time.Duration
	ErrThreshold int
	ErrCooldown  time.Duration
	DefaultModel string
}

// Gateway 千问办公渠道门面。
type Gateway struct {
	cfg Config

	loginMu sync.Mutex
	logins  map[string]*LoginTransaction

	mu             sync.Mutex
	dynamicModels  []DynamicModel
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
		cfg.DefaultModel = "flash"
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

// staticModels 抓包实测确认的目录入口（动态目录不可得时兜底）。
// 千问办公 CN CLI 默认模型 key 为 "flash"（x-model-key 实测）。
var staticModels = []DynamicModel{
	{Key: "flash", DisplayName: "Flash", IsReasoning: true, IsDefault: true},
}

func (g *Gateway) fetchDynamic() []DynamicModel {
	g.mu.Lock()
	if g.dynamicModels != nil && time.Since(g.dynamicFetched) < time.Hour {
		out := g.dynamicModels
		g.mu.Unlock()
		return out
	}
	g.mu.Unlock()

	var out []DynamicModel
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
		if err := cr.EnsureDT(g.cfg.Upstream.OpenAPI); err != nil {
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

// ModelEntries 面板/客户端模型清单（id 带 "qwenwork:" 前缀）。
func (g *Gateway) ModelEntries() []map[string]any {
	infos := g.fetchDynamic()
	if len(infos) == 0 {
		infos = staticModels
	}
	out := make([]map[string]any, 0, len(infos))
	for _, m := range infos {
		name := m.Key
		if m.DisplayName != "" {
			name = NormalizeModelName(m.DisplayName)
		}
		e := map[string]any{
			"id":           "qwenwork:" + name,
			"object":       "model",
			"created":      1753600000,
			"owned_by":     "qwenwork",
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

// resolveModelKey 客户端模型名 → 上游 key。
func (g *Gateway) resolveModelKey(model string) (string, bool) {
	model = strings.TrimSpace(model)
	if model == "" || strings.EqualFold(model, "auto") {
		model = g.cfg.DefaultModel
	}
	lookup := func(name string) (string, bool) {
		for _, m := range g.fetchDynamic() {
			client := m.Key
			if m.DisplayName != "" {
				client = NormalizeModelName(m.DisplayName)
			}
			if client == name || m.Key == name {
				return m.Key, true
			}
		}
		for _, m := range staticModels {
			client := m.Key
			if m.DisplayName != "" {
				client = NormalizeModelName(m.DisplayName)
			}
			if client == name || m.Key == name {
				return m.Key, true
			}
		}
		return "", false
	}
	if key, ok := lookup(model); ok {
		return key, true
	}
	return lookup(NormalizeModelName(model))
}

// ---------------------------------------------------------------------------
// 对话入口
// ---------------------------------------------------------------------------

// maxBodyBytes 请求体上限。
const maxBodyBytes = 8 << 20

// ChatCompletions 处理一次千问办公渠道对话（已由主路由剥 "qwenwork:" 前缀）。
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
		Model           string           `json:"model"`
		Stream          bool             `json:"stream"`
		Messages        []map[string]any `json:"messages"`
		Tools           []any            `json:"tools"`
		ReasoningEffort string           `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse body: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "messages is required")
		return
	}

	effort := NormalizeReasoningEffort(req.ReasoningEffort)
	model := bareModel
	if model == "" {
		model = req.Model
	}
	if i := strings.LastIndex(model, "|"); i >= 0 {
		if e := NormalizeReasoningEffort(model[i+1:]); e != "" {
			effort = e
		}
		model = model[:i]
	}
	modelKey, ok := g.resolveModelKey(model)
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

		if err := cr.EnsureDT(g.cfg.Upstream.OpenAPI); err != nil {
			lastErr = err
			if IsAuthInvalid(err) {
				g.cfg.Pool.Disable(acct.UID, "auth invalid (re-import required)")
			} else {
				g.cfg.Pool.Cooldown(acct.UID, cosy.CoolErr, g.cfg.ErrCooldown, "dt ensure: "+err.Error())
			}
			continue
		}

		rc, err := g.cfg.Upstream.ChatForward(acct, cr, req.Messages, modelKey, req.Tools, effort)
		if err != nil {
			lastErr = err
			var ue *cosy.Error
			if errors.As(err, &ue) {
				switch ue.Kind {
				case cosy.ErrHardCredit:
					g.cfg.Pool.Cooldown(acct.UID, cosy.CoolHard, g.cfg.HardCooldown, "余额不足")
				case cosy.ErrSoftRate:
					g.cfg.Pool.Cooldown(acct.UID, cosy.CoolSoft, g.cfg.SoftCooldown, "429 rate limit")
				case cosy.ErrTokenExpired:
					cr.mu.Lock()
					cr.ExpiresAt = time.Time{}
					cr.mu.Unlock()
					g.cfg.Pool.NoteError(acct.UID, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
				case cosy.ErrSessionDead:
					g.cfg.Pool.Disable(acct.UID, "session dead")
				case cosy.ErrNotFound:
					g.cfg.Pool.Cooldown(acct.UID, cosy.CoolSoft, g.cfg.SoftCooldown, "upstream 404")
				default:
					g.cfg.Pool.NoteError(acct.UID, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
				}
			} else {
				g.cfg.Pool.NoteError(acct.UID, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
			}
			continue
		}

		g.cfg.Pool.NoteSuccess(acct.UID)
		if req.Stream {
			// 首包门（同 qoderwork）：第一个内容帧之前不写响应头，流内错误帧在
			// 此命中即冷却换号（上游把容量/路由拒绝放在 200 的流内错误帧里）。
			verdict, pre, rest, ie, herr := cosy.PrefetchNestedSSE(rc)
			if verdict != cosy.HeadAnswered {
				rc.Close()
				if herr != nil {
					lastErr = herr
					g.cfg.Pool.NoteError(acct.UID, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
					continue
				}
				if ie != nil {
					lastErr = ie
					if ie.Busy() {
						g.busyCooldown(acct.UID, ie)
					} else {
						g.cfg.Pool.NoteError(acct.UID, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
					}
					continue
				}
				lastErr = fmt.Errorf("upstream stream ended with no content")
				g.cfg.Pool.NoteError(acct.UID, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
				continue
			}
			writeSSEHeaders(w)
			fl, _ := w.(http.Flusher)
			flush := func() {
				if fl != nil {
					fl.Flush()
				}
			}
			serr := cosy.StreamAsOpenAIPrefixed(w, pre, rest, model, flush)
			rc.Close()
			if serr != nil {
				log.Printf("qwenwork_chat uid=%s model=%s: stream error: %v", acct.UID, modelKey, serr)
				// 内容已写出后的流内错误无法再换号（响应已 200）——只记账冷却。
				var ie2 *cosy.InStreamError
				if errors.As(serr, &ie2) {
					reportOutcome(onOutcome, acct.UID, acct.Nickname, reqlog.OutcomeStreamError, http.StatusBadGateway)
					if ie2.Busy() {
						g.busyCooldown(acct.UID, ie2)
					} else {
						g.cfg.Pool.NoteError(acct.UID, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
					}
				} else {
					g.cfg.Pool.NoteError(acct.UID, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
				}
			}
			return
		}
		resp, aerr := cosy.AggregateNested(rc, model)
		rc.Close()
		if aerr != nil {
			// 换号重试而非直接 502；Busy 类短软冷却不刷错误计数
			var ie *cosy.InStreamError
			if errors.As(aerr, &ie) && ie.Busy() {
				g.busyCooldown(acct.UID, ie)
			} else {
				g.cfg.Pool.NoteError(acct.UID, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
			}
			lastErr = aerr
			continue
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	msg := "all qwenwork accounts unavailable (cooling/disabled)"
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

// busyCooldown 容量类瞬时拒绝（10605 队列满等）的短软冷却：按帧内
// retryAfterSeconds（缺省 SoftCooldown，下限 10s）让该号退出本轮轮转，
// 不刷错误计数（账号无恙，只是队列满）。
func (g *Gateway) busyCooldown(uid string, ie *cosy.InStreamError) {
	d := ie.RetryAfter()
	if d <= 0 {
		d = g.cfg.SoftCooldown
	}
	if d < 10*time.Second {
		d = 10 * time.Second
	}
	g.cfg.Pool.Cooldown(uid, cosy.CoolSoft, d, "upstream busy (in-stream)")
}

// ---------------------------------------------------------------------------
// 面板管理端点
// ---------------------------------------------------------------------------

// AccountSummary 单账号面板视图。
type AccountSummary struct {
	cosy.Status
	TokenExpiresAt int64  `json:"token_expires_at,omitempty"`
	HasRefresh     bool   `json:"has_refresh"`
	Email          string `json:"email,omitempty"`
	Username       string `json:"username,omitempty"`
}

func (g *Gateway) accountSummaries() []AccountSummary {
	list := g.cfg.Pool.List()
	out := make([]AccountSummary, 0, len(list))
	for _, st := range list {
		s := AccountSummary{Status: st}
		if acct := g.cfg.Pool.Get(st.UID); acct != nil {
			if cr, ok := acct.Data.(*Cred); ok && cr != nil {
				cr.mu.Lock()
				if !cr.ExpiresAt.IsZero() {
					s.TokenExpiresAt = cr.ExpiresAt.Unix()
				}
				s.HasRefresh = strings.TrimSpace(cr.RefreshToken) != ""
				s.Email = cr.Email
				s.Username = cr.Username
				cr.mu.Unlock()
			}
		}
		out = append(out, s)
	}
	return out
}

// AccountsHandler GET /panel/api/qwenwork/accounts
func (g *Gateway) AccountsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"accounts": g.accountSummaries()})
}

// LoginStartHandler POST /panel/api/qwenwork/login：发起设备授权，返回浏览器
// 授权 URL（CN CLI 同源登录页，登录/扫码确认后凭 nonce 轮询结果）。
func (g *Gateway) LoginStartHandler(w http.ResponseWriter, r *http.Request) {
	tx, err := StartLogin()
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "login_start_failed", err.Error())
		return
	}
	g.loginMu.Lock()
	g.logins[tx.Nonce] = tx
	g.loginMu.Unlock()
	time.AfterFunc(LoginTTL, func() {
		g.loginMu.Lock()
		delete(g.logins, tx.Nonce)
		g.loginMu.Unlock()
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"verify_url": tx.VerifyURL,
		"nonce":      tx.Nonce,
		"expires_at": tx.ExpiresAt.Unix(),
	})
}

// LoginResultHandler GET /panel/api/qwenwork/login/result?nonce=：轮询授权结果。
// pending → 202 {status:"pending"}；成功 → 落盘入池后 {status:"ok",...}；
// 未知/过期 nonce → 404 {status:"expired"}。
func (g *Gateway) LoginResultHandler(w http.ResponseWriter, r *http.Request) {
	nonce := r.URL.Query().Get("nonce")
	g.loginMu.Lock()
	tx := g.logins[nonce]
	g.loginMu.Unlock()
	if tx == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"status": "expired"})
		return
	}
	cr, err := PollLogin(tx)
	if err != nil {
		if errors.Is(err, ErrAuthPending) {
			writeJSON(w, http.StatusAccepted, map[string]any{"status": "pending"})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	g.loginMu.Lock()
	delete(g.logins, nonce)
	g.loginMu.Unlock()
	if g.cfg.AuthDir != "" {
		if serr := cr.Save(g.cfg.AuthDir); serr != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "save_failed", serr.Error())
			return
		}
	}
	g.cfg.Pool.Add(cosy.NewAccount(cr.UID, cr.Name, cr.fingerprint(), cr))
	log.Printf("qwenwork account added via login: uid=%s nick=%s", cr.UID, cr.Name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": cr.UID})
}

// LoginCancelHandler POST /panel/api/qwenwork/login/cancel：放弃授权事务。
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

// ImportHandler POST /panel/api/qwenwork/accounts/import —— 导入解密 auth-v2.dat 形态 JSON。
func (g *Gateway) ImportHandler(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req struct {
		JSON string `json:"json"`
	}
	if err := json.Unmarshal(raw, &req); err == nil && strings.TrimSpace(req.JSON) != "" {
		raw = []byte(req.JSON)
	}
	cr, err := ParseCred(raw)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_credential", err.Error())
		return
	}
	cr.completeFingerprint()
	if _, _, derr := cr.EnsureRuntime(); derr != nil {
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

// DeleteHandler DELETE /panel/api/qwenwork/accounts/{uid}
func (g *Gateway) DeleteHandler(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing uid")
		return
	}
	if g.cfg.AuthDir != "" {
		_ = os.Remove(filepath.Join(g.cfg.AuthDir, "qwenwork-"+uid+".json"))
	}
	g.cfg.Pool.Remove(uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// PatchHandler PATCH /panel/api/qwenwork/accounts/{uid}
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

// RefreshHandler POST /panel/api/qwenwork/accounts/{uid}/refresh —— 刷 JWT + 身份落盘。
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
	if err := cr.EnsureDT(g.cfg.Upstream.OpenAPI); err != nil {
		if IsAuthInvalid(err) {
			g.cfg.Pool.Disable(uid, "auth invalid (re-import required)")
			writeOpenAIError(w, http.StatusConflict, "auth_invalid", err.Error())
		} else {
			writeOpenAIError(w, http.StatusBadGateway, "refresh_failed", err.Error())
		}
		return
	}
	cr.InvalidateRuntime()
	if _, _, err := cr.EnsureRuntime(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "runtime_derive_failed", err.Error())
		return
	}
	if g.cfg.AuthDir != "" {
		_ = cr.Save(g.cfg.AuthDir)
	}
	remain, known, qerr := g.cfg.Upstream.QuotaUsage(cr.Token)
	if qerr == nil && known {
		g.cfg.Pool.ReenableIfCredits(uid, remain)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid})
}

// JSONHandler GET /panel/api/qwenwork/accounts/{uid}/json —— 原始凭证 JSON。
func (g *Gateway) JSONHandler(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if g.cfg.AuthDir != "" {
		raw, err := os.ReadFile(filepath.Join(g.cfg.AuthDir, "qwenwork-"+uid+".json"))
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

// ModelsRefreshHandler POST /panel/api/qwenwork/models/refresh
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
		"error": map[string]any{
			"message": msg,
			"type":    code,
			"code":    code,
		},
	})
}
