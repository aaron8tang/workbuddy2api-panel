// compact.go 出站请求体历史瘦身（降级路径）：在两池 PrepareBody 改写
// 完成后、出站前执行。
//
// 背景（3004 根因，2026-10-13 容器日志取证）：WorkBuddy 长会话 agent 请求
// 可达 3MB+ / 456 轮消息 / 218 次工具调用历史。SOLO 上游对超大请求一律以
// 流内 error code=3004（rate limit 文案）拒绝——同一 3MB 请求连续打挂 6 个
// 不同账号、而 2MB 合成请求可过，证明这是按请求体量（token 数）触发的
// 服务端限流，换号无效。CN 主池接入同一策略作为防御性预算控制。
//
// 策略（等价于客户端 context window 管理，服务端代理实施）：
//  1. body ≤ MaxBodyBytes 时零改动（正例零回归）；
//  2. 超预算 → 保 system 消息（前缀）+ 最后 KeepTail 条消息（近端工具往返），
//     中段从最老一侧逐条剔除，直至达标或中段耗尽；
//  3. 若单条消息即超预算（巨 blob），不进一步拆条（无安全切点），照原样
//     出站交上游裁决（出站上限仍是 gateway 的 8MB 硬限）。
package promptcache

import (
	"encoding/json"
	"log"
)

// CompactLimits 瘦身参数。
type CompactLimits struct {
	// MaxBodyBytes 出站 body 预算上限（字节）。0 = MaxCompactBodyBytes。
	MaxBodyBytes int
	// KeepTail 保底保留的消息条数（从尾部数，含最新 user 输入）。
	// 0 = DefaultKeepTail。KeepTail < 0 禁用瘦身（原样返回）。
	KeepTail int
}

const (
	// MaxCompactBodyBytes 出站 body 预算。取证数据：2MB 合成请求稳定
	// 通过，3MB 必 3004；留出余量取 2MB。
	MaxCompactBodyBytes = 2 << 20
	// DefaultKeepTail 保底尾段条数：system 之外通常是一轮 assistant 回复 +
	// 若干 tool 结果 + 最新 user 输入，取 8 覆盖一次完整工具往返。
	DefaultKeepTail = 8
)

// CompactBody 超预算时裁剪 messages 中段，返回新 body 与裁掉的条数。
// body 无法解析 / 不含 messages / 未超预算 → 原样返回。
func CompactBody(src []byte, lim CompactLimits) ([]byte, int) {
	maxBytes := lim.MaxBodyBytes
	if maxBytes <= 0 {
		maxBytes = MaxCompactBodyBytes
	}
	keepTail := lim.KeepTail
	if keepTail == 0 {
		keepTail = DefaultKeepTail
	}
	if keepTail < 0 || len(src) <= maxBytes {
		return src, 0
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(src, &obj); err != nil {
		return src, 0
	}
	rawMsgs, ok := obj["messages"]
	if !ok {
		return src, 0
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(rawMsgs, &msgs); err != nil {
		return src, 0
	}
	if len(msgs) <= keepTail {
		return src, 0
	}

	// 前缀段：开头的 system 消息（可能多条）全部保留。
	prefixEnd := 0
	for prefixEnd < len(msgs) {
		var m struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(msgs[prefixEnd], &m); err != nil || m.Role != "system" {
			break
		}
		prefixEnd++
	}
	// 尾段起点：从尾部保 keepTail 条，但不吞掉 system 前缀。
	tailStart := len(msgs) - keepTail
	if tailStart < prefixEnd {
		tailStart = prefixEnd
	}
	if tailStart <= prefixEnd {
		// 中段为空（消息全在保底范围内）——已是能保的最小形态。
		return src, 0
	}

	// 中段（可裁区）：从最老一侧逐条剔除，直至达标或中段耗尽。
	// 不做配对智能识别（assistant.tool_calls 与 tool 结果在 OpenAI 约定下
	// 连续出现，从最老侧连续剔除天然保住剩余序列的配对完整性）。
	mid := append([]json.RawMessage(nil), msgs[prefixEnd:tailStart]...)
	dropped := 0
	for len(mid) > 0 {
		// 先试裁掉当前最老一条，重估 body 大小。
		candidate := append(append([]json.RawMessage{}, msgs[:prefixEnd]...),
			append(append([]json.RawMessage{}, mid[1:]...), msgs[tailStart:]...)...)
		out := reassemble(obj, candidate)
		if len(out) <= maxBytes {
			return out, dropped + 1
		}
		mid = mid[1:]
		dropped++
	}
	// 中段裁空仍超预算（单条巨消息）→ 返回仅前缀+尾段的最小形态，
	// 由上游对残余巨消息做最终裁决。
	out := reassemble(obj, append(append([]json.RawMessage{}, msgs[:prefixEnd]...), msgs[tailStart:]...))
	return out, dropped
}

// reassemble 用新 messages 序列重组 body（其余顶层字段原样保留）。
func reassemble(obj map[string]json.RawMessage, msgs []json.RawMessage) []byte {
	obj["messages"] = mustMarshal(msgs)
	out, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	return out
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("[]")
	}
	return b
}

// LogCompactResult 裁剪发生时打一行日志（与 body profile 画像对齐）。
// channel 是通道标签（solo / cn / global…），对齐两池各自的日志前缀。
func LogCompactResult(channel string, inBytes, outBytes, dropped int) {
	if dropped > 0 {
		log.Printf("%s: compact history %d -> %d bytes (dropped %d oldest mid-segment messages)",
			channel, inBytes, outBytes, dropped)
	}
}
