// gateway.go AutoClaw 渠道门面：对话编排（凭据→刷新→体改写→转发→首包门→
// 错误分类轮转）+ 池管理 + 面板管理端点 + 手机验证码登录端点。
package autoclaw

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/stagger"
)

// Config 网关配置。
type Config struct {
	Region       Region
	AuthDir      string // autoclaw-*.json 凭证目录
	MaxRotate    int    // 0 = 3
	SoftCooldown time.Duration
	ErrThreshold int
	ErrCooldown  time.Duration
}

// Gateway 门面。
type Gateway struct {
	region   Region
	cfg      Config
	upstream *Upstream
	authDir  string

	mu      sync.Mutex
	creds   []*Credential
	cooled  cooldownState
	errCnt  map[string]int
	rf      struct {
		mu      sync.Mutex
		flights map[string]*flight
	}
	cat catalogCache

	loginMu     sync.Mutex
	loginDevice string // 当前进行中的验证码登录 device_id
	loginPhone  string // 配对的手机号（agent-login 必须回传同一号码）

	// checkinMu 签到批量互斥：面板手动端点与 scheduler 定时钩子可能同时触发
	// CheckinAll，两批交叉会同窗口打同一账号（2026-10-23 事故整改第二阶段）。
	// 忙时 TryLock 失败返回空结果（调用方按「无账号或忙」处理）。
	checkinMu sync.Mutex

	stopCh chan struct{}
}

// New 构建网关并加载凭证目录。
func New(cfg Config) *Gateway {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 10 * time.Minute // 429 无结构化恢复时间，落 10 分钟兜底
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 5 * time.Minute
	}
	if cfg.Region == "" {
		cfg.Region = RegionCN
	}
	g := &Gateway{
		region: cfg.Region, cfg: cfg, upstream: NewUpstream(), authDir: cfg.AuthDir,
		errCnt: map[string]int{}, stopCh: make(chan struct{}),
		cooled: newCooldownState(),
	}
	g.rf.flights = map[string]*flight{}
	g.ReloadAccounts()
	g.StartHourlyRefresh(g.stopCh)
	return g
}

// Close 停后台任务。
func (g *Gateway) Close() { close(g.stopCh) }

// logf 渠道日志。
func (g *Gateway) logf(format string, args ...any) {
	log.Printf("[autoclaw] "+format, args...)
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

// pickEnabled 取一个已启用且不在冷却的凭据。
func (g *Gateway) pickEnabled() *Credential {
	now := time.Now()
	for _, c := range g.List() {
		if !c.Enabled || !c.Valid() {
			continue
		}
		g.cooled.mu.Lock()
		until := g.cooled.untils[c.SafeUID()]
		g.cooled.mu.Unlock()
		if until.IsZero() || now.After(until) {
			return c
		}
	}
	return nil
}

// Servable 至少一个健康账号。
func (g *Gateway) Servable() bool { return g.pickEnabled() != nil }

// HasAccounts 是否有已启用账号。
func (g *Gateway) HasAccounts() bool {
	for _, c := range g.List() {
		if c.Enabled && c.Valid() {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 对话编排
// ---------------------------------------------------------------------------

// ChatCompletions 处理一次 AutoClaw 渠道对话（主链路 "autoclaw:" 前缀分发）。
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
	routeID, bare := g.ResolveRoute(model)

	var bmap map[string]any
	if err := json.Unmarshal(body, &bmap); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求体不是 JSON 对象: "+err.Error())
		return
	}
	normalizeRequestBody(bmap, bare)

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < g.cfg.MaxRotate; i++ {
		var cred *Credential
		now := time.Now()
		for _, c := range g.List() {
			if !c.Enabled || !c.Valid() || tried[c.SafeUID()] {
				continue
			}
			g.cooled.mu.Lock()
			until := g.cooled.untils[c.SafeUID()]
			g.cooled.mu.Unlock()
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
			nick := firstNonEmpty(cred.Name, cred.Email, cred.UserID)
			cred.mu.Unlock()
			onAccount(cred.SafeUID(), nick)
		}
		// 临期续期（一次性轮换单飞 + 写回）。
		fresh, rerr := g.EnsureFresh(cred, false)
		if rerr != nil {
			if errors.Is(rerr, ErrAuthInvalid) {
				g.cooldown(cred.SafeUID(), 365*24*time.Hour, "登录态失效，请重新登录/导入")
				lastErr = rerr
				continue
			}
			// 刷新失败但 token 未临期 → 沿用旧 token 继续（不打断在途请求）。
			if cred.NeedsRefresh(time.Now()) {
				lastErr = rerr
				g.noteError(cred.SafeUID())
				continue
			}
			g.logf("提前刷新失败（%v），旧 token 尚未过期，继续使用", rerr)
			fresh = cred
		}
		payload, err := json.Marshal(bmap)
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "serialize_failed", err.Error())
			return
		}
		rc, cerr := g.upstream.ChatForward(fresh, routeID, payload)
		if cerr != nil {
			lastErr = cerr
			if ue, ok := cerr.(*UpstreamError); ok {
				g.classifyAndCooldown(fresh.SafeUID(), ue)
				if ue.Status == http.StatusUnauthorized {
					// 401 → 强制刷一次后同账号重试（adapter.rs:341-365）。
					if _, rerr := g.EnsureFresh(fresh, true); rerr == nil {
						tried[cred.SafeUID()] = false // 允许同号再试一次
					}
				}
			} else {
				g.noteError(fresh.SafeUID())
			}
			continue
		}
		g.noteSuccess(fresh.SafeUID())
		if peek.Stream {
			// 首包门（对齐 cosy 三渠道）：第一个内容帧之前不写响应头，流内错误
			// 在此换号。AutoClaw 上游是标准 OpenAI SSE（非嵌套 envelope），
			// 用裸 SSE 判定（errFrame 形态：code+message 无 choices 同样适用）。
			verdict, pre, rest, ie, herr := prefetchOpenAISSE(rc)
			if verdict != headAnswered {
				rc.Close()
				if herr != nil {
					lastErr = herr
					g.noteError(fresh.SafeUID())
					continue
				}
				if ie != nil {
					lastErr = ie
					g.cooldownStreamFault(fresh.SafeUID(), ie)
					continue
				}
				lastErr = fmt.Errorf("upstream stream ended with no content")
				g.noteError(fresh.SafeUID())
				continue
			}
			writeSSEHeaders(w)
			flusher, _ := w.(http.Flusher)
			serr := streamOpenAIPrefixed(w, pre, rest, model, func() {
				if flusher != nil {
					flusher.Flush()
				}
			})
			rc.Close()
			if serr != nil {
				// 写出内容后的流内错误：无法换号，只记账冷却。
				g.logf("stream error uid=%s model=%s: %v", fresh.SafeUID(), bare, serr)
				if ie, ok := serr.(*InStreamErr); ok {
					reportOutcome(onOutcome, fresh.SafeUID(), nicknameOf(fresh), reqlog.OutcomeStreamError, http.StatusBadGateway)
					g.cooldownStreamFault(fresh.SafeUID(), ie)
				} else {
					g.noteError(fresh.SafeUID())
				}
			}
			return
		}
		// 非流式：聚合（未写响应，可换号）。
		out, fault := aggregateOpenAI(rc, model)
		rc.Close()
		if fault != nil {
			lastErr = fault
			if ie, ok := fault.(*InStreamErr); ok {
				g.cooldownStreamFault(fresh.SafeUID(), ie)
			} else {
				g.noteError(fresh.SafeUID())
			}
			continue
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
		return
	}
	msg := fmt.Sprintf("all autoclaw accounts unavailable (cooling/disabled)")
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

// normalizeRequestBody 出站体三处改写（adapter.rs:161-241）：
//  1. model ← 剥前缀模型 ID（上游认这个；路由 ID 已放 X-Request-Model 头）
//  2. max_tokens：存在且为正数且 <10000 → 10000（预校验墙：30~8192 小正整数
//     一律 406 空体）；缺省不注入；max_completion_tokens 一字不动
//  3. system 提示闸门（前缀注入 + 9 条身份句改写）
func normalizeRequestBody(body map[string]any, bareModel string) {
	body["model"] = bareModel
	if n, ok := body["max_tokens"].(float64); ok && n > 0 && n < 10000 {
		body["max_tokens"] = float64(10000)
	}
	if needs, _ := PromptNeedsGate(body); needs {
		ApplyPromptGate(body)
	}
}

// classifyAndCooldown 非 2xx 分类（adapter.rs:275-305）：
// 401 → 刷新后同号重试（由调用方处理，这里只冷却）；429 → 软冷却；其余 Fatal 记数。
func (g *Gateway) classifyAndCooldown(uid string, ue *UpstreamError) {
	switch ue.Status {
	case http.StatusUnauthorized:
		g.cooldown(uid, g.cfg.ErrCooldown, "401 未授权")
	case http.StatusTooManyRequests:
		g.cooldown(uid, g.cfg.SoftCooldown, "429 限流（无结构化恢复时间，10 分钟兜底）")
	case http.StatusNotAcceptable:
		// 406 空响应体：上游预校验拒收（max_tokens / system 指纹 / 风控）。
		g.cooldown(uid, g.cfg.ErrCooldown, "406 预校验拒收（检查 max_tokens 与 system 提示）")
	default:
		g.noteError(uid)
	}
}

// cooldownStreamFault 流内错误帧冷却（OpenAI error 形态 code 判定）。
func (g *Gateway) cooldownStreamFault(uid string, ie *InStreamErr) {
	if strings.Contains(ie.Code, "429") || strings.Contains(strings.ToLower(ie.Message), "rate limit") {
		g.cooldown(uid, g.cfg.SoftCooldown, "流内 429")
		return
	}
	g.noteError(uid)
}

// ---------------------------------------------------------------------------
// 池操作
// ---------------------------------------------------------------------------

func (g *Gateway) cooldown(uid string, d time.Duration, reason string) {
	g.cooled.mu.Lock()
	if g.cooled.untils == nil {
		g.cooled.untils = map[string]time.Time{} // 防御：零值 Gateway 也不能 panic
	}
	g.cooled.untils[uid] = time.Now().Add(d)
	g.cooled.mu.Unlock()
	reasonStore.Lock()
	reasonStore.m[uid] = reason
	reasonStore.Unlock()
	g.mu.Lock()
	g.errCnt[uid] = 0
	g.mu.Unlock()
}

func (g *Gateway) noteError(uid string) {
	g.mu.Lock()
	g.errCnt[uid]++
	if g.cfg.ErrThreshold > 0 && g.errCnt[uid] >= g.cfg.ErrThreshold {
		g.cooled.mu.Lock()
		g.cooled.untils[uid] = time.Now().Add(g.cfg.ErrCooldown)
		g.cooled.mu.Unlock()
		g.errCnt[uid] = 0
	}
	g.mu.Unlock()
}

func (g *Gateway) noteSuccess(uid string) {
	g.mu.Lock()
	g.errCnt[uid] = 0
	g.mu.Unlock()
}

func nicknameOf(c *Credential) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return firstNonEmpty(c.Name, c.Email, c.UserID)
}

func reportOutcome(onOutcome reqlog.OutcomeReporter, uid, nick, outcome string, status int) {
	if onOutcome != nil {
		onOutcome(uid, nick, outcome, status)
	}
}

// ---------------------------------------------------------------------------
// 面板管理端点
// ---------------------------------------------------------------------------

// StatusHandler GET /panel/api/autoclaw/status
func (g *Gateway) StatusHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": true, "servable": g.Servable(), "has_accounts": g.HasAccounts(),
	})
}

// AccountsHandler GET /panel/api/autoclaw/accounts
func (g *Gateway) AccountsHandler(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	type view struct {
		UID          string `json:"uid"`
		Nickname     string `json:"nickname,omitempty"`
		Email        string `json:"email,omitempty"`
		UserID       string `json:"user_id"`
		HasRefresh   bool   `json:"has_refresh"`
		TokenTail    string `json:"token_tail,omitempty"`
		ExpiresAtMs  int64  `json:"expires_at_ms,omitempty"`
		Cooling      bool   `json:"cooling"`
		Until        string `json:"until,omitempty"`
		Reason       string `json:"reason,omitempty"`
	}
	out := make([]view, 0)
	for _, c := range g.List() {
		uid := c.SafeUID()
		c.mu.Lock()
		v := view{
			UID: uid, Nickname: firstNonEmpty(c.Name, c.Email, c.UserID), Email: c.Email, UserID: c.UserID,
			HasRefresh: c.CanRefresh(), TokenTail: c.TokenTail, ExpiresAtMs: c.ExpiresAtMs,
		}
		c.mu.Unlock()
		g.cooled.mu.Lock()
		until := g.cooled.untils[uid]
		g.cooled.mu.Unlock()
		if !until.IsZero() && now.Before(until) {
			v.Cooling = true
			v.Until = until.Format(time.RFC3339)
			g.mu.Lock()
			v.Reason = firstNonEmpty(reasonOf(g, uid), "")
			g.mu.Unlock()
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// reasonOf 取冷却原因（简化：存 until 时同时存 reason）。
var reasonStore = struct {
	sync.Mutex
	m map[string]string
}{m: map[string]string{}}

func reasonOf(g *Gateway, uid string) string {
	reasonStore.Lock()
	defer reasonStore.Unlock()
	return reasonStore.m[uid]
}

// ImportRequest 导入请求体。
type ImportRequest struct {
	Token        string `json:"token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	Name         string `json:"name,omitempty"`
	Email        string `json:"email,omitempty"`
	// DesktopAuthJSON 可选：粘贴桌面端 auth.json 的完整内容（enc: 密文自动解密）。
	DesktopAuthJSON string `json:"desktop_auth_json,omitempty"`
	// ImportDesktop true = 直接读本机桌面端登录态（%APPDATA%\AutoClaw\auth.json），
	// 与粘贴整包等价但免手工拷贝；仅 Windows（DPAPI 解密）。
	ImportDesktop bool `json:"import_desktop,omitempty"`
}

// ImportHandler POST /panel/api/autoclaw/accounts/import
func (g *Gateway) ImportHandler(w http.ResponseWriter, r *http.Request) {
	raw, rerr := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if rerr != nil || len(raw) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing credential json")
		return
	}
	var req ImportRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse body: "+err.Error())
		return
	}
	token, refresh := strings.TrimSpace(req.Token), strings.TrimSpace(req.RefreshToken)
	// 桌面端 auth.json：整包粘贴，或直接读本机文件（import_desktop）。
	desktopRaw := strings.TrimSpace(req.DesktopAuthJSON)
	if token == "" && desktopRaw == "" && req.ImportDesktop {
		raw, derr := readDesktopAuthFile()
		if derr != nil {
			writeOpenAIError(w, http.StatusBadRequest, "desktop_unavailable", derr.Error())
			return
		}
		desktopRaw = string(raw)
	}
	if token == "" && desktopRaw != "" {
		t, rt, em, derr := tokensFromDesktopAuth([]byte(desktopRaw))
		if derr != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_credential", derr.Error())
			return
		}
		token, refresh = t, rt
		if req.Email == "" {
			req.Email = em
		}
	}
	if token == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"缺少 token（支持明文 JWT / enc: safeStorage 密文 / desktop_auth_json 整包粘贴）")
		return
	}
	cred, err := NewFromToken(token, refresh, req.DeviceID, req.Name, req.Email, "manual")
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_credential", err.Error())
		return
	}
	g.upsert(cred)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": cred.SafeUID(), "name": nicknameOf(cred)})
}

// desktopAuthPath 桌面端 auth.json 的绝对路径（仅用于面板展示，空 = 定位不到）。
func desktopAuthPath() string {
	if !isWindows() {
		return ""
	}
	dir := strings.TrimSpace(osGetenv("AUTOCLAW_USER_DATA_DIR"))
	if dir == "" {
		dir = strings.TrimSpace(osGetenv("APPDATA"))
		if dir == "" {
			return ""
		}
		dir = filepath.Join(dir, "AutoClaw")
	}
	return filepath.Join(dir, "auth.json")
}

// readDesktopAuthFile 读本机桌面端登录态文件（credentials.rs:239-278）：
// %APPDATA%\AutoClaw\auth.json（AUTOCLAW_USER_DATA_DIR 可覆盖目录）。
// 仅 Windows：token/refreshToken 是 enc: 密文，解密依赖同机的 DPAPI 密钥。
func readDesktopAuthFile() ([]byte, error) {
	if !isWindows() {
		return nil, fmt.Errorf("导入桌面端登录态仅支持 Windows（其他平台请粘贴明文 token 或 auth.json 内容）")
	}
	dir := strings.TrimSpace(osGetenv("AUTOCLAW_USER_DATA_DIR"))
	if dir == "" {
		dir = strings.TrimSpace(osGetenv("APPDATA"))
		if dir == "" {
			return nil, fmt.Errorf("读不到 APPDATA 环境变量（无法定位桌面端用户数据目录）")
		}
		dir = filepath.Join(dir, "AutoClaw")
	}
	p := filepath.Join(dir, "auth.json")
	raw, err := osReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("读不到桌面端登录态文件 %s（%v）：客户端没登录过？可改用「粘贴 auth.json 内容」", p, err)
	}
	return raw, nil
}

// tokensFromDesktopAuth 解桌面端 auth.json：{deviceId, userInfo:{email}, token, refreshToken}。
func tokensFromDesktopAuth(raw []byte) (token, refresh, email string, err error) {
	var doc struct {
		DeviceID string `json:"deviceId"`
		UserInfo struct {
			Email string `json:"email"`
		} `json:"userInfo"`
		Token        string `json:"token"`
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", "", "", fmt.Errorf("auth.json 不是合法 JSON: %w", err)
	}
	if strings.TrimSpace(doc.Token) == "" {
		return "", "", "", fmt.Errorf("auth.json 里没有 token 字段")
	}
	// enc: 前缀在这里不解（NewFromToken 统一处理）；refreshToken 同理。
	return doc.Token, doc.RefreshToken, doc.UserInfo.Email, nil
}

// DesktopStatusHandler GET /panel/api/autoclaw/desktop：本机桌面端登录态可用性探测
//（面板据此决定要不要显示「导入桌面端登录态」按钮；不读 token，只报可读性与邮箱）。
func (g *Gateway) DesktopStatusHandler(w http.ResponseWriter, r *http.Request) {
	raw, err := readDesktopAuthFile()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"available": false, "reason": err.Error(),
		})
		return
	}
	_, _, email, derr := tokensFromDesktopAuth(raw)
	if derr != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"available": false, "reason": derr.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available": true, "email": email, "user_data": desktopAuthPath(),
	})
}

// DesktopImportHandler POST /panel/api/autoclaw/desktop/import：直接导入本机桌面端
// 当前登录态（读 auth.json → DPAPI+AES-GCM 解密 → 落盘明文 token）。
func (g *Gateway) DesktopImportHandler(w http.ResponseWriter, r *http.Request) {
	raw, err := readDesktopAuthFile()
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "desktop_unavailable", err.Error())
		return
	}
	token, refresh, email, derr := tokensFromDesktopAuth(raw)
	if derr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_credential", derr.Error())
		return
	}
	cred, cerr := NewFromToken(token, refresh, "", "", email, "desktop")
	if cerr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_credential", cerr.Error())
		return
	}
	g.upsert(cred)
	g.logf("account imported from desktop auth.json: uid=%s", cred.SafeUID())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": cred.SafeUID(), "name": nicknameOf(cred)})
}

// DeleteHandler DELETE /panel/api/autoclaw/accounts/{uid}
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

// PatchHandler PATCH /panel/api/autoclaw/accounts/{uid}
func (g *Gateway) PatchHandler(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	var req PatchRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse body: "+err.Error())
		return
	}
	var cred *Credential
	for _, c := range g.List() {
		if c.SafeUID() == uid {
			cred = c
			break
		}
	}
	if cred == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "unknown account "+uid)
		return
	}
	switch req.Action {
	case "enable", "clear_cooldown":
		g.cooled.mu.Lock()
		delete(g.cooled.untils, uid)
		g.cooled.mu.Unlock()
		g.mu.Lock()
		g.errCnt[uid] = 0
		g.mu.Unlock()
	case "disable":
		g.cooldown(uid, 365*24*time.Hour, "手动禁用")
	case "refresh":
		if _, err := g.EnsureFresh(cred, true); err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "refresh_failed", err.Error())
			return
		}
	default:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "unknown action "+req.Action)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// BalanceHandler POST /panel/api/autoclaw/accounts/{uid}/balance
func (g *Gateway) BalanceHandler(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	var cred *Credential
	for _, c := range g.List() {
		if c.SafeUID() == uid {
			cred = c
			break
		}
	}
	if cred == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "unknown account "+uid)
		return
	}
	fresh, err := g.EnsureFresh(cred, false)
	if err != nil {
		fresh = cred
	}
	out, err := g.Balance(fresh)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "balance_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "balance": out})
}

// CheckinAll 全部账号签到 + 活跃触发。纯函数形态——面板端点与 scheduler 自动
// 签到共用同一份逻辑。返回 (结果, 是否至少有一个成功)。
// checkinMu 互斥：手动×定时同时触发时忙方直接返回空（nil,false）——
// 同渠道两批交叉同窗口打同一账号是风控指纹，宁可丢一轮签到。
func (g *Gateway) CheckinAll() ([]AccountCheckinResult, bool) {
	if !g.checkinMu.TryLock() {
		log.Printf("[autoclaw] checkin: 批量签到执行中，本次触发跳过（闸门互斥）")
		return nil, false
	}
	defer g.checkinMu.Unlock()
	var out []AccountCheckinResult
	anyOK := false
	first := true
	for _, cred := range g.List() {
		res := AccountCheckinResult{UID: cred.SafeUID()}
		// 账号间随机错峰（2026-10-23 事故整改）：同渠道多账号绝不批量同刻打上游。
		if !first {
			time.Sleep(stagger.Heavy())
		}
		first = false
		fresh, err := g.EnsureFresh(cred, false)
		if err != nil {
			res.Msg = err.Error()
			out = append(out, res)
			continue
		}
		// 活动福利唯一触发动作是 user-info（先触发再签到）。
		_ = g.TouchUserInfo(fresh)
		cr, err := g.Checkin(fresh)
		if err != nil {
			res.Msg = err.Error()
			out = append(out, res)
			continue
		}
		res.OK = cr.OK
		res.Already = cr.Already
		res.Reward = cr.Reward
		res.Msg = cr.Msg
		if cr.OK {
			anyOK = true
		}
		out = append(out, res)
	}
	return out, anyOK
}

// CheckinHandler POST /panel/api/autoclaw/checkin —— 全部账号签到 + 活跃触发。
func (g *Gateway) CheckinHandler(w http.ResponseWriter, r *http.Request) {
	results, _ := g.CheckinAll()
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// ModelsRefreshHandler POST /panel/api/autoclaw/models/refresh
func (g *Gateway) ModelsRefreshHandler(w http.ResponseWriter, r *http.Request) {
	entries, err := g.FetchCatalog(true)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "fetch_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(entries)})
}

// ---------------------------------------------------------------------------
// 手机验证码登录（国内版；面板两步：send-code → verify）
// ---------------------------------------------------------------------------

// LoginSendHandler POST /panel/api/autoclaw/login/send-code  body {"phone":"..."}
func (g *Gateway) LoginSendHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone string `json:"phone"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse body: "+err.Error())
		return
	}
	deviceID, err := g.SendCode(req.Phone)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "send_code_failed", err.Error())
		return
	}
	// phone 与 device_id 必须配对回传：agent-login 的 body 要 phone，而上游
	// 登录响应不回显手机号——只在 send-code 用过就没了。此前 verify 恒传空串，
	// 上游按参数错误处理（400001），表现为「手机号登录提示错误」。
	g.loginMu.Lock()
	g.loginDevice = deviceID
	g.loginPhone = strings.TrimSpace(req.Phone)
	g.loginMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "device_id": deviceID})
}

// LoginVerifyHandler POST /panel/api/autoclaw/login/verify  body {"code":"123456"}
func (g *Gateway) LoginVerifyHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code   string `json:"code"`
		Phone  string `json:"phone,omitempty"`
		Device string `json:"device_id,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse body: "+err.Error())
		return
	}
	g.loginMu.Lock()
	deviceID := firstNonEmpty(req.Device, g.loginDevice)
	phone := firstNonEmpty(req.Phone, g.loginPhone)
	g.loginMu.Unlock()
	if deviceID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "没有进行中的登录（先调 send-code）")
		return
	}
	// agent-login 的 body 必须带发码时那个手机号；缺了上游按参数错误 400001 拒收，
	// 与「验证码错误」是两码事（不要混为一谈去提示用户重输验证码）。
	if phone == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"缺少手机号（agent-login 需要它；请重新发码，或在请求体里带 phone）")
		return
	}
	cred, err := g.VerifyCode(deviceID, phone, req.Code)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "verify_failed", err.Error())
		return
	}
	g.loginMu.Lock()
	g.loginDevice = ""
	g.loginPhone = ""
	g.loginMu.Unlock()
	g.upsert(cred)
	g.logf("account added via phone-login: uid=%s", cred.SafeUID())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": cred.SafeUID(), "name": nicknameOf(cred)})
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

// writeOpenAIError 标准 OpenAI 错误体。
