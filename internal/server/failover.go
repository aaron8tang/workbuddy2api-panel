// failover.go 跨渠道失败转移。
//
// 解决的问题：此前 realm（渠道）在请求开头一次性裁决，之后整轮轮转只在**同一个
// 渠道**内换账号；渠道内换号耗尽后错误直接透传给客户端——哪怕其它渠道此刻完全
// 健康。用户看到的现象就是「同一模型连错几次就返回错误」。
//
// 设计要点：
//
//  1. 在**两个干净的时点**转移，而不是硬改各子网关：
//     (a) 点名渠道整体不可服务（未启用 / 无可用账号）—— 下方 dispatch 入口；
//     (b) 主链路（cn/global）轮转耗尽 —— chatCompletions 末端错误透传之前。
//     这两处的共同点是「尚未写出任何响应字节」，所以转移是安全的。
//     子网关一旦写了 200 头（流式已开流）就不可能再换渠道，那种情形照旧透传。
//
//  2. 候选模型来源复用**模型组的生效优先级**（groupModelPriority）：组里本来就
//     写着「每个渠道各用哪个模型」（qoderwork:glm-5.3-flash / cn:glm-5.3 / ...），
//     它是唯一知道「跨渠道该换什么模型」的地方。没有模型组时退化为「同一裸模型名
//     依次投给其它渠道」。
//
//  3. 新渠道自动纳入：候选来自渠道表（与 healthz 的 realm_servable 同口径），
//     排除名单里的渠道不参与。新增渠道只要接线进来就自动参与转移。
//
//  4. 每个渠道只给一次机会：渠道内部的换号轮转由各网关自己完成（MaxRotate 次），
//     外层不叠加次数，避免把一次请求放大成 MaxRotate × 渠道数。
package server

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/modelgroup"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// channelChat 渠道对话面（trae 与各子网关都实现该签名）。
type channelChat interface {
	ChatCompletions(w http.ResponseWriter, r *http.Request, bareModel string,
		onAccount func(uid, nick string), onOutcome reqlog.OutcomeReporter)
}

// failoverCand 一个转移候选：换个渠道、用它自己的模型。
type failoverCand struct {
	realm     string
	gw        channelChat
	bareModel string
	effort    string // 组条目 "|档位" 后缀，非空则注入 reasoning_effort
}

// channelByName 按渠道名取网关（nil = 未启用）。
//
// 必须在**具体指针类型**上判 nil 再装箱：cosyChannel/hwChannel 的返回类型是
// 接口，直接把 (*qoder.Gateway)(nil) 装箱会得到「非 nil 接口包着 nil 指针」，
// `gw == nil` 判不出来，随后的 Servable()/ChatCompletions 直接 panic（typed-nil
// 接口陷阱）。既有的分发代码能跑是因为它只在 realm 前缀命中时才走到那里，而
// 转移路径会主动遍历全部渠道名，必然踩到。
func (h *Handler) channelByName(name string) channelChat {
	switch name {
	case "trae":
		if h.cfg.Trae == nil {
			return nil
		}
		return h.cfg.Trae
	case "qoder":
		if h.cfg.Qoder == nil {
			return nil
		}
		return h.cfg.Qoder
	case "qoderwork":
		if h.cfg.QoderWork == nil {
			return nil
		}
		return h.cfg.QoderWork
	case "qwenwork":
		if h.cfg.QwenWork == nil {
			return nil
		}
		return h.cfg.QwenWork
	case "codearts":
		if h.cfg.CodeArts == nil {
			return nil
		}
		return h.cfg.CodeArts
	case "officeace":
		if h.cfg.OfficeAce == nil {
			return nil
		}
		return h.cfg.OfficeAce
	case "autoclaw":
		if h.cfg.AutoClaw == nil {
			return nil
		}
		return h.cfg.AutoClaw
	}
	return nil
}

// servableByName 渠道此刻是否可服务（有健康账号）。
func (h *Handler) servableByName(name string) bool {
	gw := h.channelByName(name)
	if gw == nil {
		return false
	}
	s, ok := gw.(interface{ Servable() bool })
	return ok && s.Servable()
}

// failoverSkip 该错误是否**不应**触发跨渠道转移（换渠道必然同样失败）。
//
// 不放行的三类：
//   - 请求级错误（参数错 / prompt 过长 / 图片无效 / 内容拦截）：错在请求本身，
//     换渠道只会把一次请求放大成 N 次；
//   - 上游超时：撞的是同一个慢上游，换渠道把客户端拖更久，还给一串健康渠道
//     喂连败计数（与主链路超时不换号同源）；
//   - skipOnCredit 为真时的积分耗尽：调用方选择「额度没了就别折腾」。
//
// 积分耗尽的默认判断是**照常转移**：各渠道额度独立，本渠道没钱不代表别的渠道
// 没钱——这正是跨渠道转移最能救场的场景。
func failoverSkip(err error, skipOnCredit bool) bool {
	var ue *upstream.Error
	if errors.As(err, &ue) {
		if failoverStopKind(ue.Kind) {
			return true
		}
		if skipOnCredit && ue.Kind == upstream.ErrHardCredit {
			return true
		}
	}
	if errors.Is(err, errUpstreamTimeout) {
		return true
	}
	return false
}

// failoverStopKind 该错误类别是否应**立即停止**而不换渠道。
//
// 这些是「请求级」错误：错在请求本身而不在账号/渠道，换渠道必然同样失败，
// 转移只会把一次请求放大成 N 次。
func failoverStopKind(kind upstream.ErrKind) bool {
	switch kind {
	case upstream.ErrBadParams, upstream.ErrPromptTooLong, upstream.ErrImageInvalid,
		upstream.ErrContentBlocked:
		return true
	}
	return false
}

// failoverExcluded 渠道是否在转移排除名单里（大小写不敏感、去空白）。
func (h *Handler) failoverExcluded(name string) bool {
	for _, ex := range h.cfg.Failover.Exclude {
		if strings.EqualFold(strings.TrimSpace(ex), name) {
			return true
		}
	}
	return false
}

// failoverOrder 渠道转移顺序（渠道表的稳定次序；用于无模型组时的退化路径）。
// cn/global 走主链路，不在此列——主链路轮转在 chatCompletions 内部，无法作为
// 一个「渠道网关」被调用。
var failoverOrder = []string{"trae", "qoderwork", "qoder", "qwenwork", "codearts", "officeace", "autoclaw"}

// failoverCandidates 生成本次请求的转移候选（已按优先级排序、已过滤不可用渠道）。
//
// grp 非空时走模型组优先级（跨渠道换模型）；否则退化为「同一裸模型名依次投给
// 其它渠道」。curRealm 是刚刚失败的渠道，不重复尝试。
func (h *Handler) failoverCandidates(grp *modelgroup.Group, curRealm, bareModel string) []failoverCand {
	var out []failoverCand
	seen := map[string]bool{curRealm: true}
	// add 按「组条目」登记一个候选（realm 已解析、渠道可用才收）。
	addEntry := func(entry string) {
		m, eff := modelgroup.SplitEffort(entry)
		realm, bare := resolveModel(m)
		if seen[realm] || realm == "cn" || realm == "global" {
			return
		}
		if h.failoverExcluded(realm) {
			return
		}
		gw := h.channelByName(realm)
		if gw == nil || !h.servableByName(realm) {
			return
		}
		seen[realm] = true
		out = append(out, failoverCand{realm: realm, gw: gw, bareModel: bare, effort: eff})
	}
	if grp != nil {
		for _, m := range h.groupModelPriority(grp) {
			addEntry(m)
		}
		return out
	}
	for _, name := range failoverOrder {
		if seen[name] || h.failoverExcluded(name) {
			continue
		}
		gw := h.channelByName(name)
		if gw == nil || !h.servableByName(name) {
			continue
		}
		seen[name] = true
		out = append(out, failoverCand{realm: name, gw: gw, bareModel: bareModel})
	}
	return out
}

// failoverDispatch 依次尝试其它渠道，直到某个渠道真的给出响应。
// 返回 true = 已有响应写给客户端（调用方必须立即 return）。
//
// 只在「尚未写出任何响应」时调用（见文件头时点 (a)(b)），因此转移是安全的。
// 某个渠道「跑了但一个字节都没写」（如账号全冷却、网关未开流就退出）也算失败，
// 继续下一个。
func (h *Handler) failoverDispatch(w http.ResponseWriter, r *http.Request, body []byte,
	grp *modelgroup.Group, curRealm, bareModel string, st *chatStat) bool {
	maxCh := h.cfg.Failover.MaxChannels
	tried := 0
	for _, c := range h.failoverCandidates(grp, curRealm, bareModel) {
		if maxCh > 0 && tried >= maxCh {
			break
		}
		tried++
		attempt := body
		if c.effort != "" {
			attempt = setReasoningEffortField(attempt, c.effort)
		}
		log.Printf("INFO: [server] failover %s -> %s (model=%s)", curRealm, c.realm, c.bareModel)
		if h.tryChannel(c.gw, w, r, attempt, c.bareModel, st) {
			return true
		}
		log.Printf("WARN: [server] failover %s produced no response, trying next channel", c.realm)
	}
	return false
}

// tryChannel 在一个渠道网关上执行一次对话，返回是否已有响应写给客户端。
//
// 判定手段是 captureResponseWriter：各子网关的错误出口不统一（有的写 503 有的
// 只 return），逐个改代价远大于此；「有没有写过任何字节/状态码」是唯一可靠且
// 与新渠道无关的区分方式。
func (h *Handler) tryChannel(gw channelChat, w http.ResponseWriter, r *http.Request,
	body []byte, bareModel string, st *chatStat) bool {
	st.logged = true // 子网关自打表格行，外层抑制防双行记账
	r.Body = io.NopCloser(bytes.NewReader(body))
	cw := newCaptureResponseWriter(w)
	gw.ChatCompletions(cw, r, bareModel, func(uid, nick string) {
		st.uid, st.nick = uid, nick
	}, st.reportOutcome)
	return cw.wrote
}

// captureResponseWriter 记录「有没有写过响应」，用于在响应头提交前判定能否换渠道。
type captureResponseWriter struct {
	http.ResponseWriter
	wrote bool
}

func newCaptureResponseWriter(w http.ResponseWriter) *captureResponseWriter {
	return &captureResponseWriter{ResponseWriter: w}
}

func (c *captureResponseWriter) WriteHeader(status int) {
	c.wrote = true
	c.ResponseWriter.WriteHeader(status)
}

func (c *captureResponseWriter) Write(b []byte) (int, error) {
	c.wrote = true
	return c.ResponseWriter.Write(b)
}

// Flush 透传（流式必需）；不置 wrote——flush 空缓冲不产生任何字节。
func (c *captureResponseWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
