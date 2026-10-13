// chat.go trae 渠道 chat 编排：模型映射 → 账号轮转 → SOLO SSE 转发/聚合 →
// Work 通道兜底 → 错误分类冷却状态机。
// 移植自 trae2api（原 internal/server/handler.go 的 chat 部分），分发边界变化：
// workbuddy server 已剥 "trae:" 前缀并完成鉴权/统计，这里只管 trae 自己的事。
package trae

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/promptcache"
	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/trae/upstream"
)

// ChatCompletions 处理一次 trae 渠道对话（已由主路由剥前缀）。
// bareModel 为 "trae:" 后的裸模型名；"" 表示按组/缺省选择（走 DefaultModel）。
// onAccount 选号上报回调（可 nil）：每次轮转/Work 兜底实际选中账号时以
// (uid, nickname) 回调，server 侧借此把账号填进面板「请求记录」的账号列
//（trae 子系统不写 reqlog，直填会 import cycle，经回调解耦）。
func (g *Gateway) ChatCompletions(w http.ResponseWriter, r *http.Request, bareModel string, onAccount func(uid, nick string)) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if len(body) > maxBodyBytes {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 8MB limit")
		return
	}
	var peek struct {
		Stream          bool   `json:"stream"`
		Model           string `json:"model"`
		Tools           []any  `json:"tools"`
		ConversationID  string `json:"conversation_id"`
		ConversationID2 string `json:"conversationId"`
	}
	_ = json.Unmarshal(body, &peek)

	// 请求级统计：出口打印表格日志
	st := newChatStat(time.Now(), body, peek.Stream)
	st.onAccount = onAccount
	defer st.done()

	// prompt_cache_key 会话源（P0 费用优化）：promptcache 统一解析链
	// （body metadata → body 顶层 → 头族 → body user → 前缀指纹），与 CN
	// 主池（handler.go）共用同一实现——同一工具在 trae 池与 CN 主池间轮询
	// 账号时，同会话 cache key 跨池恒定。此时 body 仍是原始请求体（下方
	// setModelInBody 等改写尚未发生）。
	convID := promptcache.ResolveConversation(r.Header, body)

	// 调用方显式传了裸模型（trae:xxx / 组裁决结果）优先；否则用 body 里的 model。
	model := bareModel
	if model == "" {
		model = peek.Model
	}
	configName, err := g.mapModel(model)
	if err != nil {
		st.status = http.StatusBadRequest
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// 若请求携带外部工具定义 (tools)，说明客户端期望进行 Function Calling。
	// Work 专有通道是 Trae 内部自主 Agent 回环、无法消费外部自定义 tools，若传入了
	// tools 则将 Official Work 模型对齐回原生裸模型名称（如 DeepSeek-V4-Flash），
	// 交由支持工具调用的 SOLO 裸模型通道处理。
	hasTools := len(peek.Tools) > 0
	if hasTools && strings.HasSuffix(configName, "-Official") {
		configName = strings.TrimSuffix(configName, "-Official")
	}

	body = setModelInBody(body, configName)

	// 若显式请求 Work 模型（且未携带外部自定义 tools），走 Work 通道多账号动态调度
	if isWorkModel(model) && !hasTools {
		if g.cfg.WorkMode == upstream.WorkModeDisabled || (g.cfg.WorkClient != nil && g.cfg.WorkClient.Mode() == upstream.WorkModeDisabled) {
			st.status = http.StatusForbidden
			writeOpenAIError(w, http.StatusForbidden, "work_disabled", "work channel is disabled by configuration")
			return
		}
		handled, err := g.executeWorkRequest(w, r, body, configName, peek.Stream, st)
		if !handled {
			st.status = http.StatusServiceUnavailable
			msg := "all accounts unavailable for work channel"
			if err != nil {
				msg += ": " + err.Error()
			}
			writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
		} else {
			st.status = http.StatusOK
		}
		return
	}

	// 请求级诊断：body 顶层非白名单字段扫描（定位 4028 destination-addr 类
	// 上游拒绝的触发源：agent CLI 自定义字段被透传）。
	logUnexpectedFields(body)
	// 请求体画像：定位 3004 类上游拒绝由何种请求特征触发
	// （超大 body / 超长多轮 / 工具调用历史），与失败时间线对齐。
	logRequestBodyProfile(body)

	// 在途租约管理：出口统一释放
	var heldUID string
	defer func() {
		if heldUID != "" {
			g.cfg.Pool.Release(heldUID)
		}
	}()
	acquireAcct := func(uid string) {
		if heldUID != "" {
			g.cfg.Pool.Release(heldUID)
		}
		heldUID = uid
		g.cfg.Pool.Acquire(uid)
	}

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < g.cfg.MaxRotate; i++ {
		acct := g.cfg.Pool.PickExcluding(tried)
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		acquireAcct(acct.UID)
		st.setAccount(acct.UID, acct.Nickname)

		// token 临近过期 → 先 refresh（持锁重查，避免并发重复轮换；失败冷却换号）
		refreshed, err := g.cfg.Upstream.RefreshTokenIfNeeded(acct, g.cfg.RefreshSkew)
		if err != nil {
			lastErr = err
			g.cfg.Pool.RecordError(acct.UID)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				g.cfg.Pool.Disable(acct.UID, "refresh session dead")
			} else {
				g.cfg.Pool.Cooldown(acct.UID, pool.CoolErr, g.cfg.ErrCooldown, "refresh: "+err.Error())
			}
			continue
		}
		if refreshed {
			_ = acct.SaveAtomic()
		}

		rc, status, respBody, terr := g.cfg.Upstream.ChatStream(acct, body, convID)
		if terr != nil {
			lastErr = terr
			g.cfg.Pool.RecordError(acct.UID)
			g.cfg.Pool.NoteError(acct.UID, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
			continue
		}
		if status >= 400 {
			g.cfg.Pool.RecordError(acct.UID)
			st.status = status
			kind := upstream.Classify(status, string(respBody))
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			switch kind {
			case upstream.ErrPlanLimit:
				g.cfg.Pool.Cooldown(acct.UID, pool.CoolPlan, g.cfg.PlanCooldown, "plan 权益不足")
				// Work 通道兜底消费 work_credits
				if handled, _ := g.executeWorkRequest(w, r, body, configName, peek.Stream, st); handled {
					st.status = http.StatusOK
					return
				}
				continue
			case upstream.ErrSoftRate:
				g.cfg.Pool.Cooldown(acct.UID, pool.CoolSoft, g.cfg.SoftCooldown, "429 rate limit")
				continue
			case upstream.ErrSessionDead:
				g.cfg.Pool.Disable(acct.UID, "session dead")
				continue
			case upstream.ErrNotFound:
				// 404 短冷却不累计 errCount（防雪崩）
				g.cfg.Pool.Cooldown(acct.UID, pool.CoolSoft, g.cfg.SoftCooldown, "upstream 404")
				continue
			default:
				g.cfg.Pool.NoteError(acct.UID, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
				continue
			}
		}
		if peek.Stream {
			st.status = http.StatusOK
			// HeadCapture 采样上游原始响应体前 2KB：空流诊断用（200+JSON 业务
			// 错误体在此现形，直见上游真实应答内容）。
			hc := upstream.NewHeadCapture(rc, 2048)
			statsR := newChatStatsReaderSince(hc, st.start)
			// 流内业务错误（1005 plan/5xx 等）→ 冷却账号；早错（未写过任何
			// 内容 chunk）时 StreamWithError 不写 SSE 直接返回错误 → 此处
			// 轮转下一账号重试，而非把错误透传给客户端。
			serr := upstream.StreamWithError(w, statsR, func(se *upstream.SOLOStreamError) {
				g.handleStreamError(acct.UID, se)
			})
			rc.Close()
			var serrSE *upstream.SOLOStreamError
			if errors.As(serr, &serrSE) {
				// 早错：响应头未提交，可安全换号重试（handleStreamError 已
				// 在回调里记过冷却；此处记 lastErr 供全部耗尽时报错文案）。
				lastErr = serr
				continue
			}
			if errors.Is(serr, upstream.ErrEmptyStream) {
				// 空流：上游 200 但零 SSE 事件。此前静默吞掉只发 [DONE] →
				// agent 端显示「用户已取消」、服务端记 200 成功。现在记账号
				// 错误（连续空流触发冷却换号）+ 采样诊断日志。
				g.cfg.Pool.RecordError(acct.UID)
				g.cfg.Pool.NoteError(acct.UID, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
				log.Printf("trae_chat uid=%s: upstream empty stream (no SSE events), head=%.300s", acct.UID, hc.Head())
				st.status = http.StatusBadGateway
				writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "upstream returned empty stream")
				return
			} else if serr != nil {
				// 流中途中断（客户端断开/写失败）：不算账号成功，也不吞掉。
				log.Printf("trae_chat uid=%s: stream aborted: %v", acct.UID, serr)
				g.cfg.Pool.NoteError(acct.UID, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
			} else {
				g.cfg.Pool.NoteSuccess(acct.UID)
				g.cfg.Pool.RecordSuccess(acct.UID)
				// 积分预扣：按 SOLO 计费倍率 × total_tokens 在内存即时扣减
				//（FEFO 逐包），面板 24h/48h 到期列与选号权重下一次调用即反映；
				// 估算偏差由 CreditsLoop 定期用上游权威余量差校准收敛。倍率未知
				//（RateOf=0）时跳过——扣减换算瞎猜会污染面板数字与选号。
				if tt, ok := statsR.UsageTokens(); ok {
					g.cfg.Pool.DeductCredits(acct.UID, g.RateOf(configName), int64(tt))
				}
			}
			if toks, ok := statsR.Tokens(); ok {
				st.toks = toks
			}
			st.ttfb = statsR.TTFB()
			return
		}
		resp, err := upstream.Aggregate(rc)
		rc.Close() // 已完全消费，立即释放上游连接（防轮转 continue 泄漏 body）
		if err != nil {
			g.cfg.Pool.RecordError(acct.UID)
			// 流内业务错误（如 1005 plan 权益不足）→ 冷却账号并轮转下一账号。
			var se *upstream.SOLOStreamError
			if errors.As(err, &se) {
				lastErr = err
				switch se.Kind() {
				case upstream.ErrPlanLimit:
					g.cfg.Pool.Cooldown(acct.UID, pool.CoolPlan, g.cfg.PlanCooldown, "plan 权益不足")
				default:
					g.cfg.Pool.NoteError(acct.UID, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
				}
				continue
			}
			if errors.Is(err, upstream.ErrEmptyStream) {
				// 空流（上游 200 但零 SSE 事件）：按上游账号级故障处理，计错误
				// 冷却并轮转下一账号——非流式路径尚未写响应，可以安全换号。
				lastErr = err
				g.cfg.Pool.NoteError(acct.UID, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
				continue
			}
			st.status = http.StatusBadGateway
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			return
		}
		g.cfg.Pool.NoteSuccess(acct.UID)
		g.cfg.Pool.RecordSuccess(acct.UID)
		// 积分预扣（非流式）：口径与流式路径一致，total_tokens × 倍率即时扣减。
		if tt := usageTotalTokens(resp); tt > 0 {
			g.cfg.Pool.DeductCredits(acct.UID, g.RateOf(configName), int64(tt))
		}
		st.status = http.StatusOK
		st.toks = completionTokens(resp)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	// 所有账号耗尽/冷却，尝试 Work 通道兜底消费 work_credits
	if handled, _ := g.executeWorkRequest(w, r, body, configName, peek.Stream, st); handled {
		st.status = http.StatusOK
		return
	}
	st.status = http.StatusServiceUnavailable
	msg := "all trae accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

// handleStreamError 流式响应中的上游业务错误 → pool 冷却状态机。
// 1005 / 4008 plan 权益不足 → 长冷却；4011 限流 → 短冷却；其余（5xx/参数错误等）→ 累计错误冷却。
func (g *Gateway) handleStreamError(uid string, se *upstream.SOLOStreamError) {
	log.Printf("trae_chat uid=%s: stream error code=%d msg=%.200s", uid, se.Code, se.Msg)
	switch se.Kind() {
	case upstream.ErrPlanLimit:
		g.cfg.Pool.Cooldown(uid, pool.CoolPlan, g.cfg.PlanCooldown, "plan 权益不足")
	case upstream.ErrSoftRate:
		g.cfg.Pool.Cooldown(uid, pool.CoolSoft, g.cfg.SoftCooldown, "429 rate limit")
	default:
		g.cfg.Pool.NoteError(uid, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
	}
}

// ---------------------------------------------------------------------------
// 模型映射
// ---------------------------------------------------------------------------

// isWorkModel 判断请求模型是否属于 Work 通道专有模型
func isWorkModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(m, "work") || strings.Contains(model, "Flash-Official")
}

// mapModel 将客户端传入的 model 映射为 config_name（SPEC §4.5）：
//
//	"glm-5.2"（config_name）        → 直接转发
//	"glm-5.2__dev"（内部名）        → 去掉后缀映射回 config_name
//	"auto" / ""                     → 默认模型
//	其他未知                        → 400
func (g *Gateway) mapModel(model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" || model == "auto" {
		return g.cfg.DefaultModel, nil
	}
	if strings.ToLower(model) == "work" {
		return upstream.DefaultWorkModel, nil
	}
	// 去掉内部名后缀（__dev / __max 等）
	base := model
	if i := strings.Index(model, "__"); i >= 0 {
		base = model[:i]
	}
	if g.knownModel(base) {
		return base, nil
	}
	// 宽松匹配：下划线 → 横线，大小写不敏感（deepseek_v4_pro → DeepSeek-V4-Pro）
	norm := normalizeModelName(base)
	if g.knownModel(norm) {
		return norm, nil
	}
	if isWorkModel(model) {
		return model, nil
	}
	return "", fmt.Errorf("unknown model %q", model)
}

// normalizeModelName 将下划线命名的内部名归一化为 config_name 风格（横线分隔）。
func normalizeModelName(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
	}
	return strings.Join(parts, "-")
}

// knownModel 判断 model 是否在动态/静态模型表中。
func (g *Gateway) knownModel(model string) bool {
	for _, m := range g.modelListRaw() {
		if m["id"] == model {
			return true
		}
	}
	return false
}

// setModelInBody 将 body 中 model 字段替换为 configName，并返回改写后的 body。
func setModelInBody(body []byte, configName string) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	obj["model"] = configName
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// ---------------------------------------------------------------------------
// Work 通道
// ---------------------------------------------------------------------------

// executeWorkRequest 使用账号池多账号动态轮转调度执行 Work 通道请求。
// 调度与容灾策略：
// 1. 检查 Work 通道是否被配置禁用；
// 2. 按手工使用顺序/work_credits 择优选取健康账号（排除已尝试过的账号，无会话粘性）；
// 3. 检查并按需预刷新 token；
// 4. 发起 ChatStream 调用（在 Auto 模式下优先 Native 直连，失败平滑降级 Bridge）；
// 5. 若发生故障（网络错误/429限流/401会话失效/额度不足），进入 pool 对应冷却/禁用状态机，并自动轮转下一账号；
// 6. 成功响应后，异步触发积分探测，更新账号 work_credits 状态。
// st 为调用方的请求级统计（可 nil）：选中账号时经 setAccount 上报，
// 使 Work 通道请求的面板账号列与 trae stdout 行同主通道一样有账号。
func (g *Gateway) executeWorkRequest(w http.ResponseWriter, r *http.Request, body []byte, model string, stream bool, st *chatStat) (bool, error) {
	if g.cfg.WorkMode == upstream.WorkModeDisabled || (g.cfg.WorkClient != nil && g.cfg.WorkClient.Mode() == upstream.WorkModeDisabled) {
		return false, errors.New("work channel is disabled by configuration")
	}

	workBody := body
	if !isWorkModel(model) {
		workBody = setModelInBody(body, upstream.DefaultWorkModel)
	}

	tried := map[string]bool{}
	var lastErr error

	for i := 0; i < g.cfg.MaxRotate; i++ {
		acct := g.cfg.Pool.PickWorkExcluding(tried)
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		if st != nil {
			st.setAccount(acct.UID, acct.Nickname)
		}

		refreshed, err := g.cfg.Upstream.RefreshTokenIfNeeded(acct, g.cfg.RefreshSkew)
		if err != nil {
			lastErr = err
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				g.cfg.Pool.Disable(acct.UID, "refresh session dead")
			} else {
				g.cfg.Pool.CooldownWork(acct.UID, g.cfg.ErrCooldown, "refresh: "+err.Error())
			}
			continue
		}
		if refreshed {
			_ = acct.SaveAtomic()
		}

		rc, status, respBody, terr := g.cfg.WorkClient.ChatStream(r.Context(), acct, workBody)
		if terr != nil {
			lastErr = terr
			g.cfg.Pool.NoteWorkError(acct.UID, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
			continue
		}
		if status >= 400 {
			lastErr = fmt.Errorf("work upstream status %d: %s", status, string(respBody))
			if status == 429 {
				g.cfg.Pool.CooldownWork(acct.UID, g.cfg.SoftCooldown, "work 429 rate limit")
			} else if status == 401 || status == 403 {
				g.cfg.Pool.Disable(acct.UID, "work session dead")
			} else if status == 400 && (strings.Contains(string(respBody), "credit") || strings.Contains(string(respBody), "1005")) {
				g.cfg.Pool.CooldownWork(acct.UID, g.cfg.PlanCooldown, "work_credits 余额不足")
			} else {
				g.cfg.Pool.NoteWorkError(acct.UID, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
			}
			continue
		}

		g.cfg.Pool.NoteWorkSuccess(acct.UID)

		// 异步刷新账号最新 work_credits 积分
		go func(a *auth.Auth) {
			probeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if snap, perr := g.cfg.WorkClient.ProbeCredits(probeCtx, a); perr == nil {
				g.cfg.Pool.SetWorkCredits(a.UID, snap.WorkCredits)
				if snap.WorkCredits <= 0 {
					g.cfg.Pool.CooldownWork(a.UID, g.cfg.PlanCooldown, "work_credits 余额不足")
				}
			}
		}(acct)

		if stream {
			_ = upstream.StreamWorkToOpenAI(w, rc, model, "")
			_ = rc.Close()
			return true, nil
		}

		resp, aerr := upstream.AggregateWork(rc, model, "")
		_ = rc.Close()
		if aerr != nil {
			lastErr = aerr
			g.cfg.Pool.NoteWorkError(acct.UID, g.cfg.ErrThreshold, g.cfg.ErrCooldown)
			continue
		}
		writeJSON(w, http.StatusOK, resp)
		return true, nil
	}

	if lastErr == nil {
		lastErr = errors.New("no healthy account available for work channel")
	}
	return false, lastErr
}
