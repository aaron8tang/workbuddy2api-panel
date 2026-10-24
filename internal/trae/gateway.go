// Package trae TRAE SOLO 子系统网关：把 trae2api（独立服务）以「渠道」形态
// 合并进 workbuddy2api-panel。对外由 workbuddy 主路由分发：
//
//   - /v1/models 中追加 "trae:" 前缀条目（Gateway.ModelEntries）
//   - /v1/chat/completions 的 model="trae:xxx" 由 server 分发到 Gateway.ChatCompletions
//   - 面板经 /panel/api/trae/* 管理账号（Gateway 的各 Handler 方法）
//   - 独立调度器（签到/刷新）由 main 起 goroutine 驱动
//
// 与 workbuddy 主链路完全隔离：独立账号池（trae-*.json）、独立上游客户端、
// 独立状态文件；trae 不可用（未启用/无账号）时主链路零回归。
package trae

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/upstream"
)

// Config Gateway 依赖（由 main 装配，值来自 config.json 的 trae 段）。
type Config struct {
	Pool         *pool.Pool
	Upstream     *upstream.Client
	WorkClient   *upstream.WorkClient
	AuthDir      string        // auths/ 目录，用于 import/delete 落盘 trae-*.json
	MaxRotate    int           // 单请求最多换号次数，默认 3
	PlanCooldown time.Duration // 1005 冷却，默认 12h
	SoftCooldown time.Duration // 429 冷却，默认 60s
	ErrThreshold int           // 连续错误阈值，默认 3
	ErrCooldown  time.Duration // 错误冷却，默认 10m
	RefreshSkew  time.Duration // token 预刷新窗口，默认 24h
	DefaultModel string        // 默认 glm-5.2
	// Work 通道（可选）：model 含 work / Flash-Official 时优先走 Work 通道。
	WorkBridgeURL   string
	WorkBridgeToken string
	WorkMode        upstream.WorkMode
	// CreditResetDay trae 积分月度重置日（1-28；0 = 关闭到期估算）。上游不提供
	// 积分周期数据，跨渠道「谁先到期用谁」排序按自然月重置日估算，缺省 1 号。
	CreditResetDay int
}

// maxBodyBytes 请求体大小上限（8MB），超过返回 413。
const maxBodyBytes = 8 << 20

// Gateway trae 子系统门面。
type Gateway struct {
	cfg Config

	// 回调监听端口（登录闭环 /authorize 落点；"0" 关闭独立 listener）。
	callbackPort string
	// 调度器句柄（面板「立即签到」用；main 装配后 SetScheduler 注入）。
	sched *scheduler.Scheduler

	// Web 登录 pending 态：pendingID → 登录进行中的临时上下文。
	// 回调 /authorize 捕获后标记成功；面板轮询 result 取结果。
	loginMu sync.Mutex
	logins  map[string]*pendingLogin
}

// New 构建 Gateway 并补齐默认值。
func New(cfg Config) *Gateway {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.PlanCooldown <= 0 {
		cfg.PlanCooldown = 12 * time.Hour
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 24 * time.Hour
	}
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = upstream.DefaultConfigName
	}
	if cfg.WorkClient == nil {
		var workCfg upstream.WorkClientConfig
		if cfg.WorkBridgeURL != "" {
			workCfg.BridgeURL = cfg.WorkBridgeURL
		}
		if cfg.WorkBridgeToken != "" {
			workCfg.BridgeToken = cfg.WorkBridgeToken
		}
		if cfg.WorkMode != "" {
			workCfg.Mode = cfg.WorkMode
		}
		cfg.WorkClient = upstream.NewWorkClient(workCfg)
	}
	if cfg.WorkMode == "" {
		cfg.WorkMode = cfg.WorkClient.Mode()
	}
	return &Gateway{cfg: cfg, logins: map[string]*pendingLogin{}}
}

// SetScheduler 注入调度器句柄（面板「立即签到」按钮复用调度器同一路径）。
func (g *Gateway) SetScheduler(s *scheduler.Scheduler) { g.sched = s }

// SetCallbackPort 设置登录回调监听端口（面板 StartLogin 缺省值；"0" 关闭）。
func (g *Gateway) SetCallbackPort(port string) { g.callbackPort = port }

// StartCallbackServer 起独立 :port listener 只服务 /authorize（登录闭环回调落点）。
// port 为空/"0" 时不启动。阻塞返回由调用方 goroutine 决定；本函数内部起 goroutine。
func (g *Gateway) StartCallbackServer(port string) {
	port = strings.TrimSpace(port)
	if port == "" || port == "0" {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /authorize", g.AuthorizeCallback)
	srv := &http.Server{
		// 绑全网卡（与参考实现一致）：容器部署时 TRAE 登录页在用户浏览器上、
		// 回调打到宿主机 127.0.0.1:18080，经端口映射进容器必须能被 docker-proxy
		// 访问到——绑 127.0.0.1 会让容器内 Web 登录静默失效。端点只消费真实
		// 登录回调参数（含 refreshToken），无参数即为 400，暴露面可接受。
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("[trae] 登录回调监听 http://127.0.0.1:%s/authorize", port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[trae] 登录回调监听失败: %v（Web 登录将不可用，可用粘贴回调链接方式导入）", err)
		}
	}()
}

// Pool 暴露账号池（调度器/healthz/面板状态用）。
func (g *Gateway) Pool() *pool.Pool { return g.cfg.Pool }

// Servable 报告 trae 渠道当前能否服务（healthz 用）：
// 池内存在至少一个健康（未禁用/未软关/未冷却）账号。
func (g *Gateway) Servable() bool {
	for _, st := range g.cfg.Pool.List() {
		if !st.Disabled && st.Enabled && !st.Cooling {
			return true
		}
	}
	return false
}

// HasAccounts 报告池内是否有账号（无论健康与否）。
func (g *Gateway) HasAccounts() bool { return len(g.cfg.Pool.List()) > 0 }

// EarliestCreditExpiry 返回 trae 渠道的积分到期时刻，供模型组 default 的
// 跨渠道「谁先到期用谁」排序。
// 真实数据源：健康账号的 ide_user_ent_usage 权益包明细（scheduler 积分刷新时把
// 各包 expire_time 的最小值写进池）——桌面客户端「每天过期多少积分」即这些包的
// 聚合视图，因此到期时刻与官方口径一致（签到包=领取日+31天、月赠=月末清零）。
// 无真实明细（尚未刷新过/积分刷新失败）时按配置的月度重置日（CreditResetDay，
// 缺省 1 号）估算兜底。两者都不可得（渠道不可服务/重置日关闭）时 ok=false，
// 调用方按未知处理（排已知之后，与倍率未知一致的兜底序）。
func (g *Gateway) EarliestCreditExpiry() (time.Time, bool) {
	now := time.Now()
	var best int64
	for _, st := range g.cfg.Pool.List() {
		if st.Disabled || !st.Enabled || st.EarliestCreditExpiry <= 0 {
			continue
		}
		if st.EarliestCreditExpiry <= now.Unix() {
			continue // 已过期明细（下次积分刷新前的残留）不参与，回落估算
		}
		if st.Cooling && now.Before(st.Until) {
			continue // 冷却账号不可服务，其到期时刻不参与排序
		}
		if best == 0 || st.EarliestCreditExpiry < best {
			best = st.EarliestCreditExpiry
		}
	}
	if best > 0 {
		return time.Unix(best, 0), true
	}
	if g.cfg.CreditResetDay <= 0 || !g.Servable() {
		return time.Time{}, false
	}
	return nextMonthlyReset(now, g.cfg.CreditResetDay), true
}

// HasExpiringWithin 报告 trae 渠道是否有积分将在 d 内到期（供跨渠道调度
// 「即将到期优先用完」判断）。数据源与 EarliestCreditExpiry 一致：优先取健康
// 账号权益包明细的最早到期时刻，无明细时按月度重置日估算兜底；两者都不可得
// （渠道不可服务/重置日关闭）时返回 false（按「无即将到期」处理，排已知之后）。
func (g *Gateway) HasExpiringWithin(d time.Duration) bool {
	exp, ok := g.EarliestCreditExpiry()
	if !ok {
		return false
	}
	return time.Until(exp) <= d
}

// nextMonthlyReset 返回 now 之后下一次「每月 day 日 00:00（本地时区）」时刻。
// day 超出当月天数时钳到月末（29/30/31 在平月自动回退到 28/末日）。
func nextMonthlyReset(now time.Time, day int) time.Time {
	at := func(y int, m time.Month, d int) time.Time {
		last := time.Date(y, m, 1, 0, 0, 0, 0, now.Location()).AddDate(0, 1, 0).AddDate(0, 0, -1)
		if d > last.Day() {
			d = last.Day()
		}
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	}
	cand := at(now.Year(), now.Month(), day)
	if !cand.After(now) {
		y, m := now.Year(), now.Month()
		if m == time.December {
			y, m = y+1, time.January
		} else {
			m++
		}
		cand = at(y, m, day)
	}
	return cand
}

// ---------------------------------------------------------------------------
// /v1/models 条目
// ---------------------------------------------------------------------------

// ModelEntries 返回 trae 渠道的模型条目（OpenAI /v1/models 格式，id 已带 "trae:" 前缀，
// owned_by 固定 "trae"）。动态拉取失败回退内置静态表。
// Rate>0 的模型透出 credits 字段（如 "x0.06"）——面板「积分倍率」列与
// /v1/models 消费者都读该字段；倍率未知（Rate=0）时省略，不编造。
func (g *Gateway) ModelEntries() []map[string]any {
	infos := g.fetchDynamicModels()
	var out []map[string]any
	if len(infos) > 0 {
		out = make([]map[string]any, 0, len(infos))
		for _, mi := range infos {
			e := map[string]any{
				"id":             "trae:" + mi.ID,
				"object":         "model",
				"created":        1753600000,
				"owned_by":       "trae",
				"context_length": contextOr(mi.ContextWindow),
			}
			if mi.Name != "" {
				e["name"] = mi.Name
			}
			if mi.Rate > 0 {
				e["credits"] = fmtCreditsRate(mi.Rate)
			}
			out = append(out, e)
		}
		return out
	}
	for _, m := range staticModels {
		e := map[string]any{"object": "model", "created": 1753600000, "owned_by": "trae"}
		for k, v := range m {
			if k == "id" {
				e["id"] = "trae:" + v.(string)
				continue
			}
			e[k] = v
		}
		out = append(out, e)
	}
	return out
}

func contextOr(v any) any {
	if v == nil || v == int64(0) || v == 0 {
		return 131072
	}
	return v
}

// fmtCreditsRate 把 SOLO 计费倍率格式化为上游 credits 同款展示形态
//（"x0.06" / "x1.83"），供 /v1/models 与面板「积分倍率」列直接消费。
// 与 server.fmtCreditsPrefix 的展示口径对齐：x 前缀 + 最短小数表示。
func fmtCreditsRate(rate float64) string {
	return "x" + strconv.FormatFloat(rate, 'f', -1, 64)
}

// KnownBare 判断裸模型名（去 trae: 前缀后）是否在动态/静态模型表或 Work 兜底里。
// 供 server 侧校验与模型组兜底。
func (g *Gateway) KnownBare(bare string) bool {
	if strings.ToLower(strings.TrimSpace(bare)) == "work" {
		return true
	}
	base := bare
	if i := strings.Index(bare, "__"); i >= 0 {
		base = bare[:i]
	}
	for _, m := range g.modelListRaw() {
		if m["id"] == base {
			return true
		}
	}
	return isWorkModel(bare)
}

// modelListRaw 未加前缀的模型条目列表（mapModel 校验用）。
func (g *Gateway) modelListRaw() []map[string]any {
	if infos := g.fetchDynamicModels(); len(infos) > 0 {
		out := make([]map[string]any, 0, len(infos))
		for _, mi := range infos {
			out = append(out, map[string]any{
				"id":             mi.ID,
				"object":         "model",
				"created":        1753600000,
				"owned_by":       "trae-solo",
				"context_length": contextOr(mi.ContextWindow),
			})
		}
		return out
	}
	return staticModels
}

// RateOf 返回模型的 SOLO 计费倍率（get_detail_param 的
// display_contact_config.consumption_rate.rate，走动态模型缓存）。未知模型 /
// 动态表未拉到时返回 0，调用方按「未知倍率」处理（不预扣积分、组裁决排后）。
func (g *Gateway) RateOf(model string) float64 {
	for _, mi := range g.fetchDynamicModels() {
		if mi.ID == model {
			return mi.Rate
		}
	}
	return 0
}

// 静态 SOLO 模型表（SPEC P3：32 个 config_name，来自逆向报告；动态拉取失败时回退）。
var staticModels = []map[string]any{
	{"id": "DeepSeek-V4-Flash-Official", "object": "model", "created": 1753600000, "owned_by": "trae-work", "context_length": 131072},
	{"id": "Doubao-Seed-2.1-Pro", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "seed-code-pro-0430", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "Doubao-Seed-2.1-Turbo", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "Doubao-Seed-2.0-Code", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "browser_use_subagent", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "glm-5-turbo", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "glm-5", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "DeepSeek-V4-Pro", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "DeepSeek-V4-Flash", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "kimi-k3", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "kimi-k2.7-code", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "kimi-k2.6", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "minimax-m3", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "qwen-3.7-plus", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "sagitta", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "aquila", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_gemini", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_placeholder", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_1M_text", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_1M", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_kimi", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_claude", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_gpt-5", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_no-fc", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_deepseek_chat", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_deepseek_reasoner", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_deepseek_v4", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "explore_sub_agent_v13", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "explore_sub_agent_v2", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "summary", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
}

// dynamicModelsCache 动态模型缓存（成功 1h / 失败负缓存 5min）。
// 注意：负缓存只影响 trae 模型表刷新，不影响主链路。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time
	lastFail time.Time
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

// fetchDynamicModels 从池中任一健康账号拉模型列表（get_detail_param），缓存 1h。
func (g *Gateway) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	acct := g.cfg.Pool.Pick()
	if acct == nil {
		return nil
	}
	infos, err := g.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
	return infos
}

// ---------------------------------------------------------------------------
// 通用 HTTP helpers（trae 面板端点专用；与 server 包的错误格式对齐）
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}

// decodeBody 限长读 + JSON 解码，失败写 400 并返回 err。
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse json: "+err.Error())
		return err
	}
	return nil
}

// decodeBodyOptional 读 body（限 1MB）并 JSON 解码到 v；body 为空时返回 nil 不报错。
func decodeBodyOptional(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}

// prefix 返回 s 前 n 字符（脱敏 machine/device id 展示）。
func prefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// isSessionDeadErr 粗判 refresh 失败是否 session 失效（含 401/invalid token 标记）。
func isSessionDeadErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, m := range []string{"session", "unauthorized", "401", "invalid token", "token 失效"} {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 面板端点：账号管理
// ---------------------------------------------------------------------------

// AccountSummary 列表/预览对外结构（脱敏）。
type AccountSummary struct {
	UID          string `json:"uid"`
	Nickname     string `json:"nickname,omitempty"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	Enabled      bool   `json:"enabled"`
	Disabled     bool   `json:"disabled"` // session dead 硬禁用
	Cooling      bool   `json:"cooling"`
	Reason       string `json:"reason,omitempty"`
	Credits      int64  `json:"credits"`
	ErrCount     int    `json:"err_count,omitempty"`
	// Token 有效期（Unix 秒）与是否临近过期
	ExpiresAt   int64  `json:"expires_at,omitempty"`
	ExpiredSoon bool   `json:"expired_soon,omitempty"`
	// 最早积分到期（Unix 秒）：权益包 expire_time 明细（0 = 未刷新过明细）
	CreditExpiresAt int64  `json:"credit_expires_at,omitempty"`
	// 24h/48h 内到期作废的积分合计（credit_packs 为空 = 从未刷新过明细，前端显示 —）
	Exp24h float64 `json:"exp_24h,omitempty"`
	Exp48h float64 `json:"exp_48h,omitempty"`
	// 权益包明细快照（面板「24h/48h 到期」列的原始数据源）
	CreditPacks []pool.CreditPack `json:"credit_packs,omitempty"`
	MachineID   string            `json:"machine_id,omitempty"` // 前 8 位（脱敏）
	DeviceID    string            `json:"device_id,omitempty"`  // 前 8 位（脱敏）
	HasAuth     bool               `json:"has_auth"`
}

// PoolAccountOrder 返回当前手工使用顺序（面板 traeStatus 透出给前端）。
func (g *Gateway) PoolAccountOrder() []string { return g.cfg.Pool.AccountOrder() }

// Accounts 返回脱敏账号列表（面板 status 端点与 AccountsHandler 共用）。
// 24h/48h 到期窗口按天粒度聚合 credit_packs（与主池 expWindowSumsByUID 同口径：
// 24h→窗口 1 天、48h→窗口 2 天）；无明细时不输出 exp_* 字段（区分 0 与无数据）。
func (g *Gateway) Accounts() []AccountSummary {
	statuses := g.cfg.Pool.List()
	out := make([]AccountSummary, 0, len(statuses))
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	for _, s := range statuses {
		sum := AccountSummary{
			UID:      s.UID,
			Nickname: s.Nickname,
			Enabled:  s.Enabled,
			Disabled: s.Disabled,
			Cooling:  s.Cooling,
			Reason:   s.Reason,
			Credits:  s.Credits,
			ErrCount: s.ErrCount,

			CreditExpiresAt: s.EarliestCreditExpiry,
		}
		if len(s.CreditPacks) > 0 {
			// 有明细快照 → 原样透出（前端区分「0 = 明细存在但窗口内无到期」与
			//「无 credit_packs = 从未刷新过明细（显示 —）」两种状态）。
			sum.CreditPacks = s.CreditPacks
			for _, pk := range s.CreditPacks {
				if pk.ExpireAt <= 0 || pk.Remain <= 0 {
					continue
				}
				exp := time.Unix(pk.ExpireAt, 0)
				expDay := time.Date(exp.Year(), exp.Month(), exp.Day(), 0, 0, 0, 0, now.Location())
				d := int(expDay.Sub(today).Hours() / 24)
				if d < 0 || d > 2 {
					continue
				}
				if d <= 1 {
					sum.Exp24h += pk.Remain
				}
				if d <= 2 {
					sum.Exp48h += pk.Remain
				}
			}
		}
		if a := g.cfg.Pool.AuthByUID(s.UID); a != nil {
			sum.HasAuth = true
			sum.ExpiresAt = a.ExpiresAt
			if a.ExpiresAt > 0 && a.NeedsRefresh(24*60*60) { // 24h 内过期
				sum.ExpiredSoon = true
			}
			sum.MachineID = prefix(a.MachineID, 8)
			sum.DeviceID = prefix(a.DeviceID, 8)
		}
		out = append(out, sum)
	}
	return out
}

// AccountsHandler GET /panel/api/trae/accounts：账号列表（脱敏）。
func (g *Gateway) AccountsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"accounts": g.Accounts()})
}

// ImportRequest 导入请求体。
type ImportRequest struct {
	// 三选一：
	//   CallbackURL：TRAE 登录回调链接（http://127.0.0.1:.../authorize?refreshToken=...）
	//   JSON：嵌套形（{"account":..,"auth":..}）或扁平形（{"accessToken":..,"uid":..}）
	CallbackURL string `json:"callback_url,omitempty"`
	JSON        string `json:"json,omitempty"`
	// 可选覆盖字段（导入扁平 JSON 手建场景补 machine/device）
	MachineID string `json:"machine_id,omitempty"`
	DeviceID  string `json:"device_id,omitempty"`
}

// ImportHandler POST /panel/api/trae/accounts/import：导入凭证。
// body 三选一：回调链接 / 嵌套 JSON / 扁平 JSON。
func (g *Gateway) ImportHandler(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var req ImportRequest
	trim := strings.TrimSpace(string(body))
	if strings.HasPrefix(trim, "{") {
		if err := json.Unmarshal(body, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse json: "+err.Error())
			return
		}
	} else {
		// 纯字符串 body → 当回调链接
		req.CallbackURL = trim
	}

	if g.cfg.AuthDir == "" {
		writeOpenAIError(w, http.StatusInternalServerError, "no_auth_dir", "trae auth_dir not configured")
		return
	}

	var a *auth.Auth
	switch {
	case req.CallbackURL != "":
		a, err = g.importFromCallback(req)
	case req.JSON != "":
		a, err = g.importFromJSON(req)
	default:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "need callback_url or json")
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "import_failed", err.Error())
		return
	}
	if a.FilePath == "" {
		a.FilePath = auth.FilePathFor(g.cfg.AuthDir, a.UID)
	}

	if err := os.MkdirAll(g.cfg.AuthDir, 0o755); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "mkdir_failed", err.Error())
		return
	}
	_, statErr := os.Stat(a.FilePath)
	if err := a.SaveAtomic(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	action := "created"
	if statErr == nil {
		action = "updated"
	}
	g.cfg.Pool.Add(a)

	writeJSON(w, http.StatusOK, map[string]any{
		"uid": a.UID, "nickname": a.Nickname, "action": action, "needs_check": true,
	})
}

// importFromCallback 解析回调链接 → ExchangeToken → GetUserInfo → 构造 Auth。
func (g *Gateway) importFromCallback(req ImportRequest) (*auth.Auth, error) {
	info, err := ParseCallback(req.CallbackURL)
	if err != nil {
		return nil, err
	}
	a := &auth.Auth{
		AccessToken:  info.AccessToken,
		RefreshToken: info.RefreshToken,
		UID:          info.UID,
		Nickname:     info.Nickname,
		EnterpriseID: info.EnterpriseID,
		Domain:       "trae.cn",
		ApiHost:      "https://api.trae.com.cn",
		MachineID:    req.MachineID,
		DeviceID:     req.DeviceID,
		ExpiresAt:    info.ExpiresAt,
	}
	if a.RefreshToken != "" {
		if err := g.cfg.Upstream.RefreshToken(a); err != nil {
			return nil, errTradeErr("exchange_token: " + err.Error())
		}
	}
	uid, nick, ent, err := g.cfg.Upstream.GetUserInfo(a)
	if err == nil && uid != "" {
		a.UID = uid
		if nick != "" {
			a.Nickname = nick
		}
		if ent != "" {
			a.EnterpriseID = ent
		}
	}
	return g.finalizeImport(a)
}

func errTradeErr(s string) error { return &importError{s} }

type importError struct{ msg string }

func (e *importError) Error() string { return e.msg }

// importFromJSON 解析嵌套/扁平 JSON → 构造 Auth（凭证已完整，不 ExchangeToken）。
func (g *Gateway) importFromJSON(req ImportRequest) (*auth.Auth, error) {
	a, err := auth.Parse([]byte(req.JSON))
	if err != nil {
		return nil, err
	}
	if req.MachineID != "" {
		a.MachineID = req.MachineID
	}
	if req.DeviceID != "" {
		a.DeviceID = req.DeviceID
	}
	if a.Domain == "" {
		a.Domain = "trae.cn"
	}
	if a.ApiHost == "" {
		a.ApiHost = "https://api.trae.com.cn"
	}
	return g.finalizeImport(a)
}

// finalizeImport 导入收尾：uid/token 校验 + 签到 deviceId 自动生成。
func (g *Gateway) finalizeImport(a *auth.Auth) (*auth.Auth, error) {
	if a.UID == "" {
		return nil, &importError{"cannot determine uid from callback or GetUserInfo"}
	}
	if a.AccessToken == "" {
		return nil, &importError{"no access token after exchange"}
	}
	if _, devErr := a.EnsureCheckinDeviceID(); devErr != nil {
		return nil, &importError{"ensure device id: " + devErr.Error()}
	}
	return a, nil
}

// DeleteHandler DELETE /panel/api/trae/accounts/{uid}：删池条目 + 删 auths 文件。
func (g *Gateway) DeleteHandler(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing uid")
		return
	}
	fp := auth.FilePathFor(g.cfg.AuthDir, uid)
	removed := g.cfg.Pool.Remove(uid)
	if !removed {
		// 池中无但有残留文件 → 也清掉文件（幂等）
		if _, err := os.Stat(fp); err == nil {
			_ = os.Remove(fp)
		}
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not in pool")
		return
	}
	_ = os.Remove(fp)
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "deleted": true})
}

// PatchHandler PATCH /panel/api/trae/accounts/{uid}：软开关 / nickname。
func (g *Gateway) PatchHandler(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	var req struct {
		Enabled  *bool   `json:"enabled,omitempty"`
		Nickname *string `json:"nickname,omitempty"`
	}
	if err := decodeBody(w, r, &req); err != nil {
		return
	}
	if _, ok := g.cfg.Pool.Status(uid); !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found")
		return
	}
	if req.Enabled != nil {
		reason := "user enabled"
		if !*req.Enabled {
			reason = "user disabled"
		}
		if !g.cfg.Pool.SetEnabled(uid, *req.Enabled, reason) {
			writeOpenAIError(w, http.StatusNotFound, "not_found", "account vanished")
			return
		}
	}
	if req.Nickname != nil && *req.Nickname != "" {
		a := g.cfg.Pool.AuthByUID(uid)
		if a == nil {
			writeOpenAIError(w, http.StatusNotFound, "not_found", "no auth for uid")
			return
		}
		a.Nickname = *req.Nickname
		if a.FilePath == "" {
			a.FilePath = auth.FilePathFor(g.cfg.AuthDir, uid)
		}
		_ = a.SaveAtomic()
	}
	st, _ := g.cfg.Pool.Status(uid)
	writeJSON(w, http.StatusOK, st)
}

// OrderHandler POST /panel/api/trae/accounts/order：设置手工使用顺序。
// body {"order": ["uid1", ...]}——数组序即优先序（与面板 ↑↓ 移动后的展示序一致）；
// 空数组 = 清空恢复自动模式。响应 {"account_order": [...], "order_active": bool}。
func (g *Gateway) OrderHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Order []string `json:"order"`
	}
	if err := decodeBody(w, r, &req); err != nil {
		return
	}
	g.cfg.Pool.SetAccountOrder(req.Order)
	writeJSON(w, http.StatusOK, map[string]any{
		"account_order": g.cfg.Pool.AccountOrder(),
		"order_active":  g.cfg.Pool.AccountOrderActive(),
	})
}

// RefreshHandler POST /panel/api/trae/accounts/{uid}/refresh：手动 ExchangeToken + 落盘。
func (g *Gateway) RefreshHandler(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := g.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "no auth for uid")
		return
	}
	if err := g.cfg.Upstream.RefreshToken(a); err != nil {
		if isSessionDeadErr(err) {
			g.cfg.Pool.Disable(uid, "manual refresh: session dead")
		}
		writeOpenAIError(w, http.StatusBadGateway, "refresh_failed", err.Error())
		return
	}
	if a.FilePath == "" {
		a.FilePath = auth.FilePathFor(g.cfg.AuthDir, uid)
	}
	if err := a.SaveAtomic(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	// 顺带刷新积分明细（remain 解冻 + 最早到期时刻，失败不影响刷新结果）。
	if ps, err := g.cfg.Upstream.EntPacks(a); err == nil {
		remain := int64(0)
		for _, p := range ps {
			remain += p.Limit - int64(p.Used)
		}
		g.cfg.Pool.ReenableIfCredits(uid, remain)
		g.noteCreditExpiry(uid, ps)
	}
	st, _ := g.cfg.Pool.Status(uid)
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "expires_at": a.ExpiresAt, "status": st})
}

// JSONHandler GET /panel/api/trae/accounts/{uid}/json：脱敏预览。
func (g *Gateway) JSONHandler(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := g.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "no auth for uid")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"uid":           a.UID,
		"nickname":      a.Nickname,
		"enterprise_id": a.EnterpriseID,
		"domain":        a.Domain,
		"api_host":      a.ApiHost,
		"machine_id":    prefix(a.MachineID, 8),
		"device_id":     prefix(a.DeviceID, 8),
		"access_token":  auth.MaskToken(a.JWT(), 12),
		"refresh_token": auth.MaskToken(a.RefreshTokenValue(), 12),
		"expires_at":    a.ExpiresAt,
		"file_path":     a.FilePath,
	})
}

// CreditsHandler POST /panel/api/trae/accounts/{uid}/credits：即时查积分（ug EntUsage）。
func (g *Gateway) CreditsHandler(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := g.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "no auth for uid")
		return
	}
	remain, limit, used, packs, err := g.cfg.Upstream.EntUsage(a)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "credits_failed", err.Error())
		return
	}
	g.cfg.Pool.ReenableIfCredits(uid, remain)
	// 顺带刷新最早积分到期明细（面板「积分到期」列 + 跨渠道到期排序数据源）。
	if ps, err := g.cfg.Upstream.EntPacks(a); err == nil {
		g.noteCreditExpiry(uid, ps)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"uid": uid, "remain": remain, "limit": limit, "used": used, "packs": packs,
	})
}

// noteCreditExpiry 把权益包明细里的最早未来到期时刻与明细快照写进池（0 = 全部无期限 → 清除）。
func (g *Gateway) noteCreditExpiry(uid string, packs []upstream.EntPack) {
	earliest := int64(0)
	snapshot := make([]pool.CreditPack, 0, len(packs))
	for _, p := range packs {
		if p.ExpireAt > 0 && (earliest == 0 || p.ExpireAt < earliest) {
			earliest = p.ExpireAt
		}
		snapshot = append(snapshot, pool.CreditPack{ExpireAt: p.ExpireAt, Remain: float64(p.Limit) - p.Used})
	}
	g.cfg.Pool.SetCreditExpiry(uid, earliest, snapshot)
}

// CheckinHandler POST /panel/api/trae/checkin：立即全量签到（调度器同路径）。
func (g *Gateway) CheckinHandler(w http.ResponseWriter, r *http.Request) {
	if g.sched == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_scheduler", "trae scheduler not running")
		return
	}
	go g.sched.RunCheckinNow()
	writeJSON(w, http.StatusOK, map[string]any{"started": true})
}
