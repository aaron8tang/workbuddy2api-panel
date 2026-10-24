// gateway.go QoderWork 渠道网关：/v1/chat/completions 轮转调度 + 面板管理端点。
// 轮转范式对齐 qoderwork2api internal/server/handler.go，池操作改用 internal/cosy
// 通用池（PickExcluding + Get，无租约/无 Enabled 概念）。
package qoderwork

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
	"github.com/linguo2625469/workbuddy2api-panel/internal/stagger"
)

// Config 网关配置。
type Config struct {
	Pool         *cosy.Pool
	Upstream     *Client
	AuthDir      string // qoderwork-<uid>.json 凭证目录
	MaxRotate    int
	HardCooldown time.Duration // 余额耗尽
	SoftCooldown time.Duration // 429 / 404
	ErrThreshold int           // 连续错误冷却阈值
	ErrCooldown  time.Duration
	DefaultModel string
}

// Gateway QoderWork 渠道门面。
type Gateway struct {
	cfg Config

	loginMu sync.Mutex
	logins  map[string]*LoginTransaction

	mu             sync.Mutex
	dynamicModels  []DynamicModel
	dynamicFetched time.Time

	// checkinMu 签到批量互斥：面板手动端点与 scheduler 定时钩子可能同时触发
	// CheckinAll，忙时 TryLock 失败返回空（2026-10-23 事故整改第二阶段）。
	checkinMu sync.Mutex
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
		cfg.DefaultModel = "auto"
	}
	if cfg.Upstream == nil {
		cfg.Upstream = NewClient()
	}
	g := &Gateway{cfg: cfg, logins: map[string]*LoginTransaction{}}
	g.ReloadAccounts()
	return g
}

// Pool 暴露账号池（面板/装配用）。
func (g *Gateway) Pool() *cosy.Pool { return g.cfg.Pool }

// ReloadAccounts 扫描 AuthDir 并与池对齐（新增/剔除，状态保留在 state.json）。
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
		accts = append(accts, cosy.NewAccount(cr.UID, cr.Nickname, cosy.Fingerprint{}, cr))
	}
	g.cfg.Pool.SyncToDir(accts)
}

// Servable 渠道是否可服务（至少一个账号健康：未禁用且未冷却）。
func (g *Gateway) Servable() bool {
	for _, st := range g.cfg.Pool.List() {
		if !st.Disabled && !st.Cooling {
			return true
		}
	}
	return false
}

// HasAccounts 池内是否有账号（无论健康与否）。
func (g *Gateway) HasAccounts() bool { return len(g.cfg.Pool.List()) > 0 }

// ---------------------------------------------------------------------------
// 模型目录
// ---------------------------------------------------------------------------

// staticModels 动态目录不可得时的静态兜底（客户端名 → 上游 key），
// 移植自 qoderwork2api handler.go fallbackModelMap。
var staticModels = []DynamicModel{
	{Key: "auto", DisplayName: "Auto"},
	{Key: "qmodel_preview", DisplayName: "Qwen3.8-Max-Preview"},
	{Key: "qmodel_latest", DisplayName: "Qwen3.7-Max"},
	{Key: "qmodel", DisplayName: "Qwen3.7-Plus"},
	{Key: "q36fmodel", DisplayName: "Qwen3.6-Flash"},
	{Key: "dmodel", DisplayName: "DeepSeek-V4-Pro"},
	{Key: "dfmodel", DisplayName: "DeepSeek-V4-Flash"},
	{Key: "gm51model", DisplayName: "GLM-5.2"},
	{Key: "kmodel", DisplayName: "Kimi-K2.7-Code"},
	{Key: "mmodel", DisplayName: "MiniMax-M2.7"},
}

func (g *Gateway) fetchDynamic() []DynamicModel {
	g.mu.Lock()
	if g.dynamicModels != nil && time.Since(g.dynamicFetched) < time.Hour {
		out := g.dynamicModels
		g.mu.Unlock()
		return out
	}
	// 简单防并发重复拉取：先解锁再拉（并发重复拉取无害，仅多一次上游调用）
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
		if err := cr.EnsureDT(g.cfg.Upstream.Base); err != nil {
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
		return g.dynamicModels // 负缓存：刚拉取失败过，直接回落静态
	}
	g.dynamicFetched = time.Now() // 记录失败时刻，5 分钟内不重复打上游
	return nil
}

// ModelEntries 面板/客户端模型清单（id 带 "qoderwork:" 前缀）。
func (g *Gateway) ModelEntries() []map[string]any {
	infos := g.fetchDynamic()
	var out []map[string]any
	if len(infos) == 0 {
		infos = staticModels
	}
	out = make([]map[string]any, 0, len(infos))
	for _, m := range infos {
		name := m.Key
		if m.DisplayName != "" {
			name = NormalizeModelName(m.DisplayName)
		}
		e := map[string]any{
			"id":           "qoderwork:" + name,
			"object":       "model",
			"created":      1753600000,
			"owned_by":     "qoderwork",
			"upstream_key": m.Key,
			"reasoning":    m.IsReasoning || IsReasoningModel(m.Key),
		}
		if m.DisplayName != "" {
			e["display_name"] = m.DisplayName
		}
		if m.MaxInputTokens > 0 {
			e["context_length"] = m.MaxInputTokens
		}
		if m.PriceFactor > 0 {
			e["price_factor"] = m.PriceFactor
		}
		if m.IsVL {
			e["vision"] = true
		}
		out = append(out, e)
	}
	return out
}

// resolveModelKey 把客户端模型名解析为上游 key。
// ""/"auto" → DefaultModel；先查动态表再查静态表；未知返回 false。
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
	// 宽松：下划线 → 横线（deepseek_v4_pro → deepseek-v4-pro）
	if key, ok := lookup(model); ok {
		return key, true
	}
	if key, ok := lookup(NormalizeModelName(model)); ok {
		return key, true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// 对话入口
// ---------------------------------------------------------------------------

// maxBodyBytes 请求体上限（对齐 trae/主链路 8MB）。
const maxBodyBytes = 8 << 20

// ChatCompletions 处理一次 QoderWork 渠道对话（已由主路由剥 "qoderwork:" 前缀）。
// bareModel 为前缀后的裸模型名；onAccount 选号上报回调（可 nil）。
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

	// 模型解析：调用方显式裸模型（前缀/组裁决）优先；组条目 "|档位" 后缀拆出
	// 思考强度（与 modelgroup 语法一致）；否则用 body 里的 model。
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

		if err := cr.EnsureDT(g.cfg.Upstream.Base); err != nil {
			lastErr = err
			if IsAuthInvalid(err) {
				g.cfg.Pool.Disable(acct.UID, "auth invalid (re-login required)")
			} else {
				g.cfg.Pool.Cooldown(acct.UID, cosy.CoolErr, g.cfg.ErrCooldown, "dt ensure: "+err.Error())
			}
			continue
		}

		rc, err := g.cfg.Upstream.ChatForward(acct, cr, req.Messages, modelKey, req.Tools, effort)
		if err != nil {
			lastErr = err
			var ue *cosy.Error
			switch {
			case errors.As(err, &ue):
				switch ue.Kind {
				case cosy.ErrHardCredit:
					g.cfg.Pool.Cooldown(acct.UID, cosy.CoolHard, g.cfg.HardCooldown, "余额不足")
				case cosy.ErrSoftRate:
					g.cfg.Pool.Cooldown(acct.UID, cosy.CoolSoft, g.cfg.SoftCooldown, "429 rate limit")
				case cosy.ErrTokenExpired:
					cr.mu.Lock()
					cr.DTExpiresAt = 0
					cr.mu.Unlock()
					g.cfg.Pool.NoteError(acct.UID, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
				case cosy.ErrSessionDead:
					g.cfg.Pool.Disable(acct.UID, "session dead")
				case cosy.ErrNotFound:
					g.cfg.Pool.Cooldown(acct.UID, cosy.CoolSoft, g.cfg.SoftCooldown, "upstream 404")
				default:
					g.cfg.Pool.NoteError(acct.UID, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
				}
			default:
				g.cfg.Pool.NoteError(acct.UID, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
			}
			continue
		}

		g.cfg.Pool.NoteSuccess(acct.UID)
		if req.Stream {
			// 首包门：拿到第一个内容帧之前不写响应头。上游把容量/路由类拒绝
			// 放在 HTTP 200 的流内错误帧里（10605 队列满、4028 拒收等）——
			// 不设门就只能把 200 连同错误帧透传给客户端，白白扔掉换号机会
			// （用户看到连续「200 流错误」）。门内命中错误/空回答 ⇒ 本号冷却
			// 后 continue 换号重试。
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
				// 空回答（流干净结束但零内容）：换号重试。
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
				log.Printf("qoderwork_chat uid=%s model=%s: stream error: %v", acct.UID, modelKey, serr)
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
		rc.Close() // 已完全消费，立即释放上游连接（防轮转 continue 泄漏 body）
		if aerr != nil {
			// 非流式路径尚未写响应，换号重试而非直接 502；Busy 类短软冷却不刷错误计数
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
	msg := "all qoderwork accounts unavailable (cooling/disabled)"
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

// AccountSummary 单账号面板视图（Status + 凭证侧补充字段）。
type AccountSummary struct {
	cosy.Status
	DTExpiresAt int64  `json:"dt_expires_at,omitempty"`
	HasDRT      bool   `json:"has_drt"`
	Plan        string `json:"plan,omitempty"`
}

func (g *Gateway) accountSummaries() []AccountSummary {
	list := g.cfg.Pool.List()
	out := make([]AccountSummary, 0, len(list))
	for _, st := range list {
		s := AccountSummary{Status: st}
		if acct := g.cfg.Pool.Get(st.UID); acct != nil {
			if cr, ok := acct.Data.(*Cred); ok && cr != nil {
				cr.mu.Lock()
				s.DTExpiresAt = cr.DTExpiresAt
				s.HasDRT = cr.DRT != ""
				cr.mu.Unlock()
			}
		}
		out = append(out, s)
	}
	return out
}

// AccountsHandler GET /panel/api/qoderwork/accounts
func (g *Gateway) AccountsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"accounts": g.accountSummaries()})
}

// ImportRequest 面板导入请求体：凭证 JSON（嵌套或扁平形态均可）。
type ImportRequest struct {
	JSON string `json:"json"`
}

// LoginStartHandler POST /panel/api/qoderwork/login：发起设备授权，返回浏览器
// 授权 URL（用户在页面登录/扫码确认后，凭 nonce 轮询结果）。
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

// LoginResultHandler GET /panel/api/qoderwork/login/result?nonce=：轮询授权结果。
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
	g.admitCred(w, cr, "login")
}

// LoginCancelHandler POST /panel/api/qoderwork/login/cancel：放弃授权事务。
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

// admitCred 凭证入渠三连：补昵称 → 落盘 → 入池（import 与 login 共用收尾）。
func (g *Gateway) admitCred(w http.ResponseWriter, cr *Cred, via string) {
	if g.cfg.AuthDir != "" {
		if err := cr.Save(g.cfg.AuthDir); err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "save_failed", err.Error())
			return
		}
	}
	g.cfg.Pool.Add(cosy.NewAccount(cr.UID, cr.Nickname, cosy.Fingerprint{}, cr))
	log.Printf("qoderwork account added via %s: uid=%s nick=%s", via, cr.UID, cr.Nickname)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": cr.UID})
}

// ImportHandler POST /panel/api/qoderwork/accounts/import
func (g *Gateway) ImportHandler(w http.ResponseWriter, r *http.Request) {
	var req ImportRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || strings.TrimSpace(req.JSON) == "" {
		// 兼容直接上传凭证 JSON 本体（非包一层 {json:...}）
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
	g.admitCred(w, cr, "import")
}

// DeleteHandler DELETE /panel/api/qoderwork/accounts/{uid}
func (g *Gateway) DeleteHandler(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing uid")
		return
	}
	if g.cfg.AuthDir != "" {
		_ = os.Remove(filepath.Join(g.cfg.AuthDir, "qoderwork-"+uid+".json"))
	}
	g.cfg.Pool.Remove(uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// PatchRequest 面板账号操作请求。
type PatchRequest struct {
	Action   string `json:"action"` // enable | disable | clear_cooldown
	Nickname string `json:"nickname"`
}

// PatchHandler PATCH /panel/api/qoderwork/accounts/{uid}
func (g *Gateway) PatchHandler(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	var req PatchRequest
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
	if req.Nickname != "" {
		if acct := g.cfg.Pool.Get(uid); acct != nil {
			acct.Nickname = req.Nickname
			if cr, ok := acct.Data.(*Cred); ok && cr != nil {
				cr.Nickname = req.Nickname
			}
			g.cfg.Pool.SaveState()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// RefreshHandler POST /panel/api/qoderwork/accounts/{uid}/refresh
// 刷新 dt + 积分 + 套餐，并把凭证写回 AuthDir。
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
	if err := cr.EnsureDT(g.cfg.Upstream.Base); err != nil {
		if IsAuthInvalid(err) {
			g.cfg.Pool.Disable(uid, "auth invalid (re-login required)")
			writeOpenAIError(w, http.StatusConflict, "auth_invalid", err.Error())
		} else {
			writeOpenAIError(w, http.StatusBadGateway, "refresh_failed", err.Error())
		}
		return
	}
	if g.cfg.AuthDir != "" {
		_ = cr.Save(g.cfg.AuthDir)
	}
	remain, _, err := g.cfg.Upstream.QuotaUsage(cr.DT)
	if err == nil {
		g.cfg.Pool.ReenableIfCredits(uid, remain)
	}
	plan, _ := g.cfg.Upstream.UserPlan(cr.DT)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     err == nil,
		"uid":    uid,
		"credit": remain,
		"plan":   plan,
	})
}

// JSONHandler GET /panel/api/qoderwork/accounts/{uid}/json —— 原始凭证 JSON。
func (g *Gateway) JSONHandler(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if g.cfg.AuthDir != "" {
		raw, err := os.ReadFile(filepath.Join(g.cfg.AuthDir, "qoderwork-"+uid+".json"))
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

// ModelsRefreshHandler POST /panel/api/qoderwork/models/refresh —— 强制刷新动态目录。
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

// CreditsHandler POST /panel/api/qoderwork/accounts/{uid}/credits —— 手动刷新积分。
func (g *Gateway) CreditsHandler(w http.ResponseWriter, r *http.Request) {
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
	if err := cr.EnsureDT(g.cfg.Upstream.Base); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "refresh_failed", err.Error())
		return
	}
	detail, err := g.cfg.Upstream.QuotaDetail(cr.DT)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "quota_failed", err.Error())
		return
	}
	remain := int64(detail.Available())
	g.cfg.Pool.SetCredits(uid, remain)
	if detail.Exceeded {
		g.cfg.Pool.Cooldown(uid, cosy.CoolHard, g.cfg.HardCooldown, "余额不足")
	}
	out := map[string]any{"ok": true, "credit": remain, "exceeded": detail.Exceeded,
		"available": detail.Available()}
	// buckets 空数组整键不写（对齐 agent2api「空数组不写键」纪律）。
	var buckets []map[string]any
	if detail.User.Total != nil || detail.User.Used != nil || detail.User.Remaining != nil {
		b := map[string]any{"name": "套餐额度"}
		if detail.User.Total != nil {
			b["total"] = *detail.User.Total
		}
		if detail.User.Used != nil {
			b["used"] = *detail.User.Used
		}
		if detail.User.Remaining != nil {
			b["remain"] = *detail.User.Remaining
		}
		buckets = append(buckets, b)
	}
	if detail.AddOnUsable() {
		b := map[string]any{"name": "个人资源包"}
		if detail.AddOn.Total != nil {
			b["total"] = *detail.AddOn.Total
		}
		if detail.AddOn.Used != nil {
			b["used"] = *detail.AddOn.Used
		}
		if detail.AddOn.Remaining != nil {
			b["remain"] = *detail.AddOn.Remaining
		}
		buckets = append(buckets, b)
	}
	if detail.OrgUsable() {
		b := map[string]any{"name": "组织资源包"}
		if detail.Org.Cap != nil {
			b["total"] = *detail.Org.Cap
		} else if detail.Org.Total != nil {
			b["total"] = *detail.Org.Total
		}
		if detail.Org.Used != nil {
			b["used"] = *detail.Org.Used
		}
		if detail.Org.Remaining != nil {
			b["remain"] = *detail.Org.Remaining
		}
		buckets = append(buckets, b)
	}
	if len(buckets) > 0 {
		out["buckets"] = buckets
	}
	if detail.ExpiresAt != nil {
		ms := *detail.ExpiresAt
		// 到期哨兵 253402214400000（9999-12-31）与 ≤0 按「无到期」不显示。
		if ms > 0 && ms < 4102444800000 {
			out["expires_at"] = ms
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// CheckinResult 单账号签到结果。
type CheckinResult struct {
	UID string `json:"uid"`
	OK  bool   `json:"ok"`
	Msg string `json:"msg,omitempty"`
	// Already 今天已领取；NoClaim 暂无可领取活动（均不算失败）。
	Already bool `json:"already,omitempty"`
	NoClaim bool `json:"no_claim,omitempty"`
}

// CheckinAll 全部账号签到。纯函数形态——面板端点与 scheduler 自动签到共用同一份
// 逻辑（此前内联在 HTTP handler 里）。
func (g *Gateway) CheckinAll() []CheckinResult {
	if !g.checkinMu.TryLock() {
		log.Printf("[qoderwork] checkin: 批量签到执行中，本次触发跳过（闸门互斥）")
		return nil
	}
	defer g.checkinMu.Unlock()
	var out []CheckinResult
	first := true
	for _, st := range g.cfg.Pool.List() {
		res := CheckinResult{UID: st.UID}
		acct := g.cfg.Pool.Get(st.UID)
		cr, ok := acct.Data.(*Cred)
		if acct == nil || !ok || cr == nil {
			res.Msg = "no credential"
			out = append(out, res)
			continue
		}
		if err := cr.EnsureDT(g.cfg.Upstream.Base); err != nil {
			res.Msg = err.Error()
			out = append(out, res)
			continue
		}
		// 账号间随机错峰（2026-10-23 事故整改）：同渠道多账号绝不批量同刻打上游。
		if !first {
			time.Sleep(stagger.Heavy())
		}
		first = false
		outcome, err := g.cfg.Upstream.CheckinV2(cr.DT)
		if err != nil {
			res.Msg = errString(err)
			out = append(out, res)
			continue
		}
		res.OK = outcome.OK
		res.Already = outcome.Already
		res.NoClaim = outcome.NoClaim
		res.Msg = outcome.Msg
		if outcome.OK {
			if remain, _, qerr := g.cfg.Upstream.QuotaUsage(cr.DT); qerr == nil {
				g.cfg.Pool.ReenableIfCredits(st.UID, remain)
			}
		}
		out = append(out, res)
	}
	return out
}

// CheckinHandler POST /panel/api/qoderwork/checkin —— 全部账号签到。
func (g *Gateway) CheckinHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"results": g.CheckinAll()})
}

// ---------------------------------------------------------------------------
// HTTP 小工具（与 trae/gateway.go 同款）
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

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
