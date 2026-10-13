// compact_summary.go 两段式 compaction（P1）：摘要前置 + 稳定裁剪点。
//
// 动机（与 CompactBody 滑动裁剪的关键差异）：
// agent 第 N 轮请求与前 N-1 轮共享长前缀（system + 完整历史），上游本可
// 用 prompt_cache_key 命中前缀缓存（费用降 ~17×，cachekey.go P0）。但
// CompactBody 每轮从最老侧滑动裁剪 → 裁剪点每轮前移 → 前缀全部失效，
// P0 的 cache key 形同虚设。
//
// 两段式目标态：会话内锁定一个稳定裁剪点 cut，被裁中段折叠成一条确定性
// 摘要插在 cut 位置，出站形态恒为：
//
//	[system..., summary, msgs[cut:]]
//
// 后续轮次只追加尾部新消息 → `[system + summary + msgs[cut:]]` 前缀跨轮
// 字节一致 → P0 cache key 真正命中。
//
// 摘要策略：抽取式确定性摘要（无 LLM 调用、无热路径延迟、无账号消耗、
// 天然幂等）——每条被裁消息取 role + 工具名 + 内容前缀，总量封顶。
// 先例：Claude Code compaction 也是把摘要作为 user 消息前置。
//
// 降级：JSON 解析失败 / 无 conversationID（无法锁会话缓存）→ 返回 nil，
// 调用方回落 CompactBody 滑动裁剪行为。
//
// 分桶（跨池合并新增）：summaryCache 按 channel 分桶。entry 的
// firstKeptFP 是对 POST-PrepareBody 消息字节算的，而两池的 PrepareBody
// 改写语义不同（SOLO 白名单重建 vs CN map 原地改写）→ 跨池指纹必然
// 失配，不分桶会互相覆盖、反复重算。channel ∈ solo / cn / global。
package promptcache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

// summaryCacheEntry 会话级稳定裁剪状态。
type summaryCacheEntry struct {
	// cutIndex 保留序列起始绝对索引（msgs[cutIndex:] 保留）。
	cutIndex int
	// firstKeptFP msgs[cutIndex] 原始 JSON 的 sha256[:16] hex——校验本轮
	// 历史 msgs[cutIndex] 与缓存时是同一条消息（客户端重写/截断历史时
	// 指纹不匹配 → 缓存作废重算，绝不裁错位置）。
	firstKeptFP string
	// summary 被裁中段的抽取式摘要（POST-PrepareBody 形态的单条消息 JSON）。
	summary json.RawMessage
	// droppedCount 生成该摘要时折叠的消息条数（返回值/日志用）。
	droppedCount int
	expiresAt    time.Time
}

const (
	// summaryCacheTTL 会话裁剪状态缓存时长。长 agent 会话通常分钟级连续
	// 请求，6h 覆盖整段工作会话；过期后下一轮重算（裁剪点可能移动一次，
	// 前缀缓存失配一轮，可接受）。
	summaryCacheTTL = 6 * time.Hour
	// summaryCacheMax 惰性清理上限（按 channel|conversationID 键；摘要
	// 内容派生、账号无关，无跨账号泄露面）。
	summaryCacheMax = 4096
	// summaryBodyFrac 摘要总量占预算的比例上限：预算越小摘要越短，
	// 保证预算主要留给保留消息（2MB 预算 → 摘要 ≤256KB，覆盖 ~1300 条
	// 消息的要点；小预算测试同样自适应）。
	summaryBodyFrac = 8
	// summaryMsgBudget 每条被裁消息的内容前缀字符数（rune 计）。
	summaryMsgBudget = 200
)

var (
	summaryMu    sync.Mutex
	summaryCache = map[string]*summaryCacheEntry{}
)

// cacheKey 分桶键：channel + conversationID。
func summaryCacheKey(channel, conversationID string) string {
	return channel + "|" + conversationID
}

// CompactBodyWithSummary 两段式瘦身：命中会话缓存 → 复用稳定裁剪点与摘要；
// 未命中 → 从 prefixEnd 起找新裁剪点（不落 tool 消息上）、生成摘要并缓存。
// channel 是通道标签（solo / cn / global），同时用作日志前缀与缓存分桶键。
// 返回 (新 body, 折叠条数)。无法处理时返回 (nil, 0)，调用方回落 CompactBody。
func CompactBodyWithSummary(src []byte, lim CompactLimits, channel, conversationID string) ([]byte, int) {
	if conversationID == "" {
		return nil, 0
	}
	maxBytes := lim.MaxBodyBytes
	if maxBytes <= 0 {
		maxBytes = MaxCompactBodyBytes
	}
	keepTail := lim.KeepTail
	if keepTail == 0 {
		keepTail = DefaultKeepTail
	}
	if keepTail < 0 || len(src) <= maxBytes {
		return nil, 0
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(src, &obj); err != nil {
		return nil, 0
	}
	rawMsgs, ok := obj["messages"]
	if !ok {
		return nil, 0
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(rawMsgs, &msgs); err != nil {
		return nil, 0
	}

	prefixEnd := countSystemPrefix(msgs)
	maxCut := len(msgs) - keepTail
	if maxCut <= prefixEnd {
		return nil, 0 // 中段为空，无可折叠
	}
	// 裁剪点目标预算：留 headroom（预算 1/4）吸收后续轮次的尾部增长，
	// 否则每轮追加后立即复验失败 → cut 每轮前移，退化为滑动裁剪。
	targetBytes := maxBytes - maxBytes/4

	cacheKey := summaryCacheKey(channel, conversationID)
	summaryMu.Lock()
	entry := summaryCache[cacheKey]
	if entry != nil && time.Now().After(entry.expiresAt) {
		delete(summaryCache, cacheKey)
		entry = nil
	}
	summaryMu.Unlock()

	// 命中：指纹校验通过（cutIndex 仍 ≥ prefixEnd 且首条保留消息未变）且
	// 重组后仍在预算内 → 复用稳定裁剪点（前缀跨轮字节一致的关键）。
	if entry != nil && entry.cutIndex >= prefixEnd && entry.cutIndex < len(msgs) &&
		entry.firstKeptFP == msgFP(msgs[entry.cutIndex]) {
		keep := append(append([]json.RawMessage(nil), msgs[:prefixEnd]...),
			append([]json.RawMessage{entry.summary}, msgs[entry.cutIndex:]...)...)
		out := reassemble(obj, keep)
		if len(out) <= maxBytes {
			return out, entry.droppedCount
		}
		// 复用后仍超预算（尾部单轮暴涨）→ 落到重算路径推进裁剪点。
	}

	// 未命中/失配/复用超预算：从 prefixEnd 起找新稳定裁剪点。
	// 尺寸估算（避免 O(n²) 重编排）：
	//   out ≈ fixedPart（顶层其余字段+框架） + prefix 消息字节 + 摘要字节
	//         + msgs[cut:] 字节
	// 其中各段消息字节用后缀和 O(n) 预计算（每条 +1 分隔符）。
	fixedPart := len(src) - len(rawMsgs) - 1
	suffix := make([]int, len(msgs)+1)
	for i := len(msgs) - 1; i >= 0; i-- {
		suffix[i] = suffix[i+1] + len(msgs[i]) + 1
	}
	prefixBytes := suffix[0] - suffix[prefixEnd]
	// 摘要封顶随预算缩放（2MB → 256KB；测试小预算按比例缩小）。
	summaryCap := targetBytes / summaryBodyFrac
	// 估算用摘要按最大覆盖范围（msgs[prefixEnd:maxCut]）取，保守高估。
	summaryBytes := len(buildSummaryMessageCapped(msgs[prefixEnd:maxCut], summaryCap)) + 256

	cut := prefixEnd
	for cut < maxCut {
		if fixedPart+prefixBytes+summaryBytes+suffix[cut] <= targetBytes {
			break
		}
		cut++
		// 裁剪点不得落在 tool 消息上（OpenAI 约定 tool 结果必须紧跟含
		// tool_calls 的 assistant 消息，保留序列首条不能是孤立 tool 结果）。
		for cut < maxCut && msgRole(msgs[cut]) == "tool" {
			cut++
		}
	}
	if cut <= prefixEnd {
		// 中段裁空仍超预算（尾部巨消息）→ 两段式无法给出更优形态，
		// 回落旧 CompactBody 行为（仅前缀+尾段）。
		return nil, 0
	}

	build := func(c int) ([]byte, json.RawMessage) {
		summary := buildSummaryMessageCapped(msgs[prefixEnd:c], summaryCap)
		keep := append(append([]json.RawMessage(nil), msgs[:prefixEnd]...),
			append([]json.RawMessage{summary}, msgs[c:]...)...)
		return reassemble(obj, keep), summary
	}
	out, summary := build(cut)
	// 估算偏差兜底：从 cut 再推进直到达标（不得越过 maxCut——尾段保底
	// 是硬约束；仍超则回落旧 CompactBody 最小形态）。
	for cut < maxCut && len(out) > targetBytes {
		cut++
		for cut < maxCut && msgRole(msgs[cut]) == "tool" {
			cut++
		}
		out, summary = build(cut)
	}
	if len(out) > targetBytes {
		return nil, 0
	}

	// 缓存稳定裁剪状态（含摘要原始 JSON）。
	entry = &summaryCacheEntry{
		cutIndex:     cut,
		firstKeptFP:  msgFP(msgs[cut]),
		summary:      summary,
		droppedCount: cut - prefixEnd,
		expiresAt:    time.Now().Add(summaryCacheTTL),
	}
	summaryMu.Lock()
	if len(summaryCache) >= summaryCacheMax {
		// 惰性清理：过期项优先，仍满则牺牲任意若干（非精确 LRU，够用）。
		now := time.Now()
		for k, v := range summaryCache {
			if now.After(v.expiresAt) {
				delete(summaryCache, k)
			}
		}
		for k := range summaryCache {
			if len(summaryCache) < summaryCacheMax {
				break
			}
			delete(summaryCache, k)
		}
	}
	summaryCache[cacheKey] = entry
	summaryMu.Unlock()

	log.Printf("%s: compact+summary conv=%s cut=%d folded=%d in=%d out=%d bytes",
		channel, truncateConv(conversationID, 24), cut, cut-prefixEnd, len(src), len(out))
	return out, cut - prefixEnd
}

// countSystemPrefix 统计开头连续 system 消息条数。
func countSystemPrefix(msgs []json.RawMessage) int {
	n := 0
	for n < len(msgs) && msgRole(msgs[n]) == "system" {
		n++
	}
	return n
}

// msgRole 取单条消息的 role（解析失败返回空串）。
func msgRole(m json.RawMessage) string {
	var v struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(m, &v); err != nil {
		return ""
	}
	return v.Role
}

// msgFP 消息指纹：sha256(raw)[:16] hex。校验客户端历史未被重写。
func msgFP(m json.RawMessage) string {
	sum := sha256.Sum256(m)
	return hex.EncodeToString(sum[:16])
}

// buildSummaryMessageCapped 把被裁中段折叠成一条确定性摘要消息（user
// 角色，POST-PrepareBody 形态 content 数组）。总量封顶 capBytes（随预算
// 缩放：预算越小摘要越短，预算主要留给保留消息）。
func buildSummaryMessageCapped(msgs []json.RawMessage, capBytes int) []byte {
	var b strings.Builder
	b.WriteString("[会话历史压缩摘要] 以下 ")
	b.WriteString(strconv.Itoa(len(msgs)))
	b.WriteString(" 条较早消息已折叠，仅保留要点：\n")
	for i, m := range msgs {
		if b.Len() >= capBytes {
			b.WriteString("…（其余省略）")
			break
		}
		var v struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			ToolCalls []struct {
				FunctionCall struct {
					Name string `json:"name"`
				} `json:"function_call"`
			} `json:"tool_calls"`
		}
		if err := json.Unmarshal(m, &v); err != nil {
			continue
		}
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(". ")
		b.WriteString(v.Role)
		for _, tc := range v.ToolCalls {
			if tc.FunctionCall.Name != "" {
				b.WriteString("(调用 ")
				b.WriteString(tc.FunctionCall.Name)
				b.WriteString(")")
			}
		}
		b.WriteString(": ")
		b.WriteString(summarizeContent(v.Content, summaryMsgBudget))
		b.WriteString("\n")
	}
	msg := map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "text", "text": b.String()},
		},
	}
	out, err := json.Marshal(msg)
	if err != nil {
		return json.RawMessage(`{"role":"user","content":[{"type":"text","text":"[会话历史压缩摘要]"}]}`)
	}
	return out
}

// summarizeContent 抽取消息内容文本（OpenAI 字符串或数组形态均支持），
// 截前 budget 个 rune。非文本内容（图片等）以占位符表示。
func summarizeContent(content json.RawMessage, budget int) string {
	if len(content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return truncateRunes(s, budget)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if sb.Len() >= budget {
				break
			}
			switch p.Type {
			case "text":
				sb.WriteString(p.Text)
			case "image_url", "image":
				sb.WriteString("[图片]")
			default:
				sb.WriteString("[" + p.Type + "]")
			}
			sb.WriteString(" ")
		}
		return truncateRunes(sb.String(), budget)
	}
	return truncateRunes(string(content), budget)
}

// truncateRunes 按 rune 截断（不撕裂多字节字符）。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// truncateConv 日志用的字节截断（对齐原 trae/upstream truncate 行为）。
func truncateConv(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
