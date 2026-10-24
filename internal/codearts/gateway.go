// gateway.go CodeArts 渠道门面：ChatCompletions（主链路分发入口）+ 模型目录
// 缓存 + 面板管理端点。
package codearts

import (
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
	Pool       *Pool
	Upstream   *Upstream
	AuthDir    string // codearts-<uid>.json 凭证目录
	MaxRotate  int    // 单请求最多换号次数；0 = 3
	CallbackPort string // Web 登录回调监听端口；空/"0" 不启动
	ErrThreshold int
	ErrCooldown  time.Duration
	QuotaCooldown time.Duration // 403 额度耗尽冷却
	RateCooldown  time.Duration // 429 限流冷却
}

// Gateway CodeArts 渠道门面。
type Gateway struct {
	cfg     Config
	pool    *Pool
	upstream *Upstream
	authDir string
	reg     *Registry
	rf      struct {
		mu      sync.Mutex
		flights map[string]*flight
	}
	gate *sessionGate

	loginMu sync.Mutex
	logins  map[string]*LoginTransaction

	// checkinMu 签到批量互斥：面板手动端点与 scheduler 定时钩子可能同时触发
	// CheckinAll，忙时 TryLock 失败返回空（2026-10-23 事故整改第二阶段）。
	checkinMu sync.Mutex

	mu             sync.Mutex
	catalog        *Catalog
	catalogErr     string
	catalogFetched time.Time
}

// New 构建网关。
func New(cfg Config) *Gateway {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 5 * time.Minute
	}
	if cfg.QuotaCooldown <= 0 {
		cfg.QuotaCooldown = 10 * time.Minute
	}
	if cfg.RateCooldown <= 0 {
		cfg.RateCooldown = 2 * time.Minute
	}
	if cfg.Upstream == nil {
		cfg.Upstream = NewUpstream()
	}
	if cfg.Pool == nil {
		cfg.Pool = NewPool()
	}
	g := &Gateway{
		cfg: cfg, pool: cfg.Pool, upstream: cfg.Upstream, authDir: cfg.AuthDir,
		reg: NewRegistry(), gate: &sessionGate{limit: 3, used: map[string]int{}},
		logins: map[string]*LoginTransaction{},
	}
	g.rf.flights = map[string]*flight{}
	g.ReloadAccounts()
	return g
}

// ReloadAccounts 扫描 AuthDir 对齐池。
func (g *Gateway) ReloadAccounts() {
	if g.authDir == "" {
		return
	}
	creds, err := LoadDir(g.authDir)
	if err != nil {
		return
	}
	g.pool.SyncToDir(creds)
}

// Pool 暴露账号池。
func (g *Gateway) PoolRef() *Pool { return g.pool }

// Servable 至少一个健康账号。
func (g *Gateway) Servable() bool {
	for _, st := range g.pool.List() {
		if !st.Disabled && !st.Cooling {
			return true
		}
	}
	return false
}

// HasAccounts 池内是否有账号。
func (g *Gateway) HasAccounts() bool { return len(g.pool.List()) > 0 }

// ---------------------------------------------------------------------------
// 模型目录（缓存 5 分钟 / 负缓存 30 秒）
// ---------------------------------------------------------------------------

func (g *Gateway) fetchCatalog(force bool) *Catalog {
	g.mu.Lock()
	if !force && g.catalog != nil && time.Since(g.catalogFetched) < 5*time.Minute {
		out := g.catalog
		g.mu.Unlock()
		return out
	}
	neg := g.catalog == nil && g.catalogErr != "" && time.Since(g.catalogFetched) < 30*time.Second
	g.mu.Unlock()
	if neg {
		return nil
	}
	_, cred := g.pool.Pick(nil)
	if cred == nil {
		return nil
	}
	// 临期先续。
	cred, err := g.RefreshSingleFlight(cred.SafeUID(), cred, false)
	if err != nil {
		g.mu.Lock()
		g.catalogErr = err.Error()
		g.catalogFetched = time.Now()
		g.mu.Unlock()
		return nil
	}
	cat := g.upstream.Discover(cred)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.catalogFetched = time.Now()
	if len(cat.Models) == 0 {
		g.catalogErr = strings.Join(cat.Warnings, "; ")
		return nil
	}
	g.catalogErr = ""
	g.catalog = cat
	return cat
}

// ModelEntries /v1/models 与面板模型清单（id 带 "codearts:" 前缀）。
func (g *Gateway) ModelEntries() []map[string]any {
	cat := g.fetchCatalog(false)
	if cat == nil {
		return nil
	}
	out := make([]map[string]any, 0, len(cat.Models))
	for _, m := range cat.Models {
		e := map[string]any{
			"id":       "codearts:" + m.ID,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "codearts",
			"source":   string(m.Source),
		}
		if m.DisplayName != "" {
			e["display_name"] = m.DisplayName
		}
		if m.ContextLength > 0 {
			e["context_length"] = m.ContextLength
		}
		if m.MaxOutputTokens > 0 {
			e["max_output_tokens"] = m.MaxOutputTokens
		}
		if m.SupportsImages {
			e["vision"] = true
		}
		if m.CreditDisplay != "" {
			e["credits"] = m.CreditDisplay
		}
		out = append(out, e)
	}
	return out
}

// benefitSource 当前目录里该模型是否福利源（决定 maas_type 头）。
func (g *Gateway) benefitSource(model string) bool {
	cat := g.fetchCatalog(false)
	if cat == nil {
		return false
	}
	if m, ok := cat.Resolve(model); ok {
		return m.Source == SourceBenefit
	}
	return false
}

// ---------------------------------------------------------------------------
// 对话入口（主链路 "codearts:" 前缀分发到这里）
// ---------------------------------------------------------------------------

// ChatCompletions 处理一次 CodeArts 渠道对话。
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
	// 大小写无关解析到上游真名（客户端习惯写小写）。
	cat := g.fetchCatalog(false)
	if cat != nil {
		if m, ok := cat.Resolve(model); ok {
			model = m.ID
		}
	}
	stream := peek.Stream

	tried := map[string]bool{}
	var lastErr string
	for i := 0; i < g.cfg.MaxRotate; i++ {
		uid, cred := g.pool.Pick(tried)
		if cred == nil {
			break
		}
		tried[uid] = true
		// 临期续期（单飞 + 写回）。
		fresh, rerr := g.RefreshSingleFlight(uid, cred, false)
		if rerr != nil {
			lastErr = rerr.Error()
			g.pool.Disable(uid, "refresh failed: "+rerr.Error())
			continue
		}
		cred = fresh
		if onAccount != nil {
			onAccount(uid, cred.UserName)
		}
		// 本地会话闸 + 上游心跳会话。
		h, gerr := g.gate.acquire(BaseURL, cred.Identity())
		if gerr != nil {
			lastErr = gerr.Error()
			writeOpenAIError(w, http.StatusConflict, "session_concurrency_limit", gerr.Error())
			return
		}
		if berr := g.upstream.Heartbeat(cred, h.id, "busy"); berr != nil {
			// 首次 busy 失败也要注销刚造的 id（上游可能已占槽）。
			_ = g.upstream.Heartbeat(cred, h.id, "idle")
			h.release()
			lastErr = berr.Error()
			g.pool.NoteError(uid, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
			continue
		}
		rc, fault, cerr := g.chatWithAccount(body, model, stream, g.benefitSource(model), cred, h.id)
		if cerr != nil {
			_ = g.upstream.Heartbeat(cred, h.id, "idle")
			h.release()
			lastErr = cerr.Error()
			g.pool.NoteError(uid, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
			continue
		}
		if fault != nil {
			_ = g.upstream.Heartbeat(cred, h.id, "idle")
			h.release()
			g.applyFault(uid, fault)
			if onOutcome != nil {
				onOutcome(uid, cred.UserName, "stream_error", fault.Status)
			}
			if stream {
				writeSSEHeaders(w)
				writeSSEError(w, fault.Status, fault.Message)
				return
			}
			// 非流式可换号重试。
			lastErr = fault.Message
			continue
		}
		g.pool.NoteSuccess(uid)
		if stream {
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
			_ = g.upstream.Heartbeat(cred, h.id, "idle")
			h.release()
			return
		}
		out, fault := Aggregate(rc, model)
		_ = g.upstream.Heartbeat(cred, h.id, "idle")
		h.release()
		if fault != nil {
			g.applyFault(uid, fault)
			if onOutcome != nil {
				onOutcome(uid, cred.UserName, "stream_error", fault.Status)
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
		"all codearts accounts unavailable (cooling/disabled)"+ifNotEmpty(": "+lastErr))
}

func ifNotEmpty(s string) string {
	if s == ": " || s == "" {
		return ""
	}
	return s
}

// applyFault 按错误分类冷却账号。
func (g *Gateway) applyFault(uid string, fault *Fault) {
	switch fault.Status {
	case 403:
		g.pool.Cooldown(uid, g.cfg.QuotaCooldown, fault.Message)
	case 429:
		g.pool.Cooldown(uid, g.cfg.RateCooldown, fault.Message)
	default:
		g.pool.NoteError(uid, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
	}
}

// ---------------------------------------------------------------------------
// 面板管理端点
// ---------------------------------------------------------------------------

// StatusHandler GET /panel/api/codearts/status
func (g *Gateway) StatusHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": true, "servable": g.Servable(), "has_accounts": g.HasAccounts(),
	})
}

// AccountsHandler GET /panel/api/codearts/accounts
func (g *Gateway) AccountsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"accounts": g.pool.List()})
}

// ImportRequest 导入请求体。
type ImportRequest struct {
	JSON string `json:"json"`
}

// ImportHandler POST /panel/api/codearts/accounts/import
func (g *Gateway) ImportHandler(w http.ResponseWriter, r *http.Request) {
	var req ImportRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || strings.TrimSpace(req.JSON) == "" {
		raw, rerr := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if rerr != nil || len(raw) == 0 {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing credential json")
			return
		}
		req.JSON = string(raw)
	}
	cr, err := ParseCred([]byte(req.JSON))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_credential", err.Error())
		return
	}
	g.admitCred(w, cr)
}

// admitCred 落盘 + 入池。
func (g *Gateway) admitCred(w http.ResponseWriter, cr *Credential) {
	if g.authDir != "" {
		if err := cr.SaveAtomic(g.authDir); err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "save_failed", err.Error())
			return
		}
	}
	g.pool.SyncToDir([]*Credential{cr})
	logf("account added: uid=%s user=%s", cr.SafeUID(), cr.UserName)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": cr.SafeUID(), "name": cr.UserName})
}

// DeleteHandler DELETE /panel/api/codearts/accounts/{uid}
func (g *Gateway) DeleteHandler(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	g.pool.Remove(uid)
	// 凭证文件删除走 os.Remove；池对齐下次 Reload。
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// PatchRequest 账号操作。
type PatchRequest struct {
	Action string `json:"action"` // enable | disable | clear_cooldown
}

// PatchHandler PATCH /panel/api/codearts/accounts/{uid}
func (g *Gateway) PatchHandler(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	var req PatchRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse body: "+err.Error())
		return
	}
	switch req.Action {
	case "enable":
		g.pool.Enable(uid)
	case "disable":
		g.pool.Disable(uid, "manual disable")
	case "clear_cooldown":
		g.pool.ClearCooldown(uid)
	default:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "unknown action "+req.Action)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// RefreshHandler POST /panel/api/codearts/accounts/{uid}/refresh —— 强制续期。
func (g *Gateway) RefreshHandler(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	cred := g.pool.Get(uid)
	if cred == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "unknown account "+uid)
		return
	}
	fresh, err := g.RefreshSingleFlight(uid, cred, true)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "refresh_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "uid": uid,
		"expires_at": fresh.ExpiresAt,
	})
}

// JSONHandler GET /panel/api/codearts/accounts/{uid}/json
func (g *Gateway) JSONHandler(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	cred := g.pool.Get(uid)
	if cred == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "unknown account "+uid)
		return
	}
	raw, _ := json.Marshal(cred)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(raw)
}

// BalanceHandler POST /panel/api/codearts/accounts/{uid}/balance —— 余额两本账。
func (g *Gateway) BalanceHandler(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	cred := g.pool.Get(uid)
	if cred == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "unknown account "+uid)
		return
	}
	fresh, err := g.RefreshSingleFlight(uid, cred, false)
	if err != nil {
		fresh = cred
	}
	stats, statErr := g.upstream.FetchStatistics(fresh)
	benefit, benErr := g.upstream.FetchBenefitBalance(fresh)
	// benefit 档案缺失自动注册一次（幂等），再重查。
	if benErr == nil && benefit == nil {
		if _, cerr := g.upstream.ClaimBenefit(fresh); cerr == nil {
			benefit, benErr = g.upstream.FetchBenefitBalance(fresh)
		}
	}
	out := map[string]any{"ok": statErr == nil || benErr == nil, "uid": uid}
	if stats != nil {
		out["statistics"] = stats
	}
	if statErr != nil {
		out["statistics_error"] = statErr.Error()
	}
	if benefit != nil {
		out["benefit"] = benefit
	} else if benErr == nil {
		out["benefit_absent"] = true
	}
	if benErr != nil {
		out["benefit_error"] = benErr.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

// CheckinResult 单账号每日动作结果（CodeArts 无「每日签到」，每日动作是福利池
// 档案注册/领取——缺失时自动注册一次，幂等）。
type CheckinResult struct {
	UID string `json:"uid"`
	OK  bool   `json:"ok"`
	Msg string `json:"msg,omitempty"`
}

// CheckinAll 全部账号执行每日福利动作：查福利池 → 缺失则幂等注册 → 重查。
// 纯函数形态——面板端点与 scheduler 自动签到共用同一份逻辑。
func (g *Gateway) CheckinAll() []CheckinResult {
	if !g.checkinMu.TryLock() {
		log.Printf("[codearts] checkin: 批量签到执行中，本次触发跳过（闸门互斥）")
		return nil
	}
	defer g.checkinMu.Unlock()
	var out []CheckinResult
	first := true
	for _, uid := range g.pool.UIDs() {
		cred := g.pool.Get(uid)
		if cred == nil || !cred.Valid() {
			continue
		}
		// 账号间随机错峰（2026-10-23 事故整改）：同渠道多账号绝不批量同刻打上游。
		if !first {
			time.Sleep(stagger.Heavy())
		}
		first = false
		res := CheckinResult{UID: uid}
		fresh, err := g.RefreshSingleFlight(uid, cred, false)
		if err != nil {
			res.Msg = err.Error()
			out = append(out, res)
			continue
		}
		benefit, benErr := g.upstream.FetchBenefitBalance(fresh)
		// 档案缺失自动注册一次（幂等），再重查。
		if benErr == nil && benefit == nil {
			if _, cerr := g.upstream.ClaimBenefit(fresh); cerr == nil {
				benefit, benErr = g.upstream.FetchBenefitBalance(fresh)
				if benErr == nil && benefit != nil {
					res.OK = true
					res.Msg = "福利池已注册"
					out = append(out, res)
					continue
				}
			}
		}
		if benErr != nil {
			res.Msg = benErr.Error()
			out = append(out, res)
			continue
		}
		if benefit == nil {
			res.Msg = "无福利池（该账号不适用）"
			out = append(out, res)
			continue
		}
		res.OK = true
		res.Msg = fmt.Sprintf("福利日池 %d/%s", benefit.DailyTokensUsed,
			func() string {
				if benefit.DailyTokenLimit > 0 {
					return fmt.Sprint(benefit.DailyTokenLimit)
				}
				return "无上限"
			}())
		out = append(out, res)
	}
	return out
}

// CheckinHandler POST /panel/api/codearts/checkin —— 全部账号每日福利动作。
func (g *Gateway) CheckinHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"results": g.CheckinAll()})
}

// ModelsRefreshHandler POST /panel/api/codearts/models/refresh
func (g *Gateway) ModelsRefreshHandler(w http.ResponseWriter, r *http.Request) {
	cat := g.fetchCatalog(true)
	if cat == nil {
		writeOpenAIError(w, http.StatusBadGateway, "fetch_failed", "catalog unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(cat.Models)})
}

// LoginStartHandler POST /panel/api/codearts/login
func (g *Gateway) LoginStartHandler(w http.ResponseWriter, r *http.Request) {
	port := g.cfg.CallbackPort
	if port == "" || port == "0" {
		writeOpenAIError(w, http.StatusConflict, "callback_disabled",
			"Web 登录回调未启用（codearts.callback_port 未配置）")
		return
	}
	tx, err := StartLogin(port)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "login_start_failed", err.Error())
		return
	}
	g.reg.Add(tx)
	g.loginMu.Lock()
	g.logins[tx.TicketID] = tx
	g.loginMu.Unlock()
	time.AfterFunc(LoginTTL, func() {
		g.reg.Drop(tx.TicketID)
		g.loginMu.Lock()
		delete(g.logins, tx.TicketID)
		g.loginMu.Unlock()
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"verify_url": AuthorizeURL(tx.Ctx, tx.CallbackURL, tx.TicketID),
		"nonce":      tx.TicketID,
		"expires_at": tx.CreatedAt.Add(LoginTTL).Unix(),
	})
}

// LoginResultHandler GET /panel/api/codearts/login/result?nonce=
func (g *Gateway) LoginResultHandler(w http.ResponseWriter, r *http.Request) {
	nonce := r.URL.Query().Get("nonce")
	g.loginMu.Lock()
	tx := g.logins[nonce]
	g.loginMu.Unlock()
	if tx == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"status": "expired"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "pending"})
}

// LoginCancelHandler POST /panel/api/codearts/login/cancel
func (g *Gateway) LoginCancelHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Nonce string `json:"nonce"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req)
	g.reg.Drop(req.Nonce)
	g.loginMu.Lock()
	delete(g.logins, req.Nonce)
	g.loginMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// HandleCallback GET /oauth/callback —— portal 回调（免鉴权，对齐 agent2api）。
// 第一趟带 secret+redirect（307 跳回 portal 给的 redirect，host 白名单校验），
// 第二趟带 code（按 ticket/候选逐个试 verifier 换码）。
func (g *Gateway) HandleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code, secret, redirect, ticket := ParseCallback(q)
	logf("callback: has_code=%v has_secret=%v ticket=%s", code != "", secret != "", ifStr(ticket))
	if code != "" {
		g.settleByCode(w, ticket, code)
		return
	}
	if secret != "" {
		if redirect != "" {
			if !trustedRedirect(redirect) {
				writeOpenAIError(w, http.StatusBadRequest, "bad_redirect",
					"回调要求跳转到的地址不是华为云站点，已拒绝")
				return
			}
			http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "secret_received"})
		return
	}
	writeOpenAIError(w, http.StatusBadRequest, "invalid_callback",
		"回调里没有授权码也没有 secret：请把浏览器地址栏里那条回调地址完整粘贴过来")
}

func ifStr(s string) string {
	if s == "" {
		return "—"
	}
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// trustedRedirect 只认华为云自己的站点（解析 host，防 open redirect）。
func trustedRedirect(raw string) bool {
	u, err := parseURLHost(raw)
	if err != nil {
		return false
	}
	return u == "huaweicloud.com" || strings.HasSuffix(u, ".huaweicloud.com")
}

func parseURLHost(raw string) (string, error) {
	rest, ok := strings.CutPrefix(raw, "https://")
	if !ok {
		rest, ok = strings.CutPrefix(raw, "http://")
		if !ok {
			return "", fmt.Errorf("bad scheme")
		}
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	// 去 userinfo。
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	host := strings.ToLower(rest)
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	if host == "" {
		return "", fmt.Errorf("no host")
	}
	return host, nil
}

// settleByCode 授权码通道：先按 ticket 精确试，再按「最近发起」逐个试。
func (g *Gateway) settleByCode(w http.ResponseWriter, ticket, code string) {
	var ordered []*LoginTransaction
	if ticket != "" {
		if tx := g.reg.Peek(ticket); tx != nil {
			ordered = append(ordered, tx)
		}
	}
	for _, tx := range g.reg.Candidates() {
		if tx.TicketID != ticket {
			ordered = append(ordered, tx)
		}
	}
	if len(ordered) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"status": "error",
			"error": "这一轮登录已作废（超过 10 分钟或已被取消），请回到面板重新发起"})
		return
	}
	var last string
	for _, tx := range ordered {
		cred, err := ExchangeCode(code, tx.CallbackURL, tx.Ctx)
		if err != nil {
			logf("换码候选 %s 未通过: %v", ifStr(tx.TicketID), err)
			last = err.Error()
			continue
		}
		// 成功才取走（授权码与 verifier 都是一次性的）。
		g.reg.Take(tx.TicketID)
		g.loginMu.Lock()
		delete(g.logins, tx.TicketID)
		g.loginMu.Unlock()
		writeHTML(w, "登录成功，账号已加入列表，可以关闭此页面。")
		go func(cr *Credential) {
			if g.authDir != "" {
				_ = cr.SaveAtomic(g.authDir)
			}
			g.pool.SyncToDir([]*Credential{cr})
			log.Printf("[codearts] account added via web-login: uid=%s user=%s", cr.SafeUID(), cr.UserName)
		}(cred)
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]any{"status": "error",
		"error": "授权码换取凭证失败: " + last})
}

// StartCallbackServer 独立监听回调端口（全网卡，容器端口映射依赖；对齐 trae）。
func (g *Gateway) StartCallbackServer(port string) {
	if port == "" || port == "0" {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/callback", g.HandleCallback)
	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 15 * time.Second}
	go func() {
		logf("callback listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logf("callback server: %v", err)
		}
	}()
}

// ---------------------------------------------------------------------------
// HTTP 小工具
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeHTML(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("<!doctype html><meta charset=utf-8><title>CodeArts 登录</title><p>" + msg + "</p>"))
}

func writeSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

// writeSSEError 以 SSE data 帧形态输出错误（流式已 200 后的失败上报）。
func writeSSEError(w http.ResponseWriter, status int, msg string) {
	code := classifyFaultCode(status, msg)
	payload := map[string]any{"error": map[string]any{
		"message": msg, "type": code, "code": code,
	}}
	raw, _ := json.Marshal(payload)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": code, "code": code},
	})
}
