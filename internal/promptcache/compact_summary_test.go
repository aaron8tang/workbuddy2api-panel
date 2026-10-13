// compact_summary_test.go 两段式 compaction（P1）测试：稳定裁剪点复用 /
// 前缀跨轮字节一致 / 工具配对约束 / 指纹失配重算 / 降级回落 / 通道分桶。
package promptcache

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// resetSummaryCache 清空会话摘要缓存（测试隔离用）。
func resetSummaryCache() {
	summaryMu.Lock()
	summaryCache = map[string]*summaryCacheEntry{}
	summaryMu.Unlock()
}

// buildAgentTurns 构造一个 agent 会话形态的 body：system 前缀 + rounds 轮
//（user → assistant(tool_calls) → tool → assistant 结果），filler 控制体量。
func buildAgentTurns(rounds int, filler string) []byte {
	var sb strings.Builder
	sb.WriteString(`{"model":"glm-5.2","messages":[{"role":"system","content":"sys-prompt"}`)
	for i := 0; i < rounds; i++ {
		sb.WriteString(`,{"role":"user","content":"` + filler + `"}`)
		sb.WriteString(`,{"role":"assistant","tool_calls":[{"function_call":{"name":"read_file","arguments":"{}"}}],"content":null}`)
		sb.WriteString(`,{"role":"tool","content":"` + filler + `"}`)
		sb.WriteString(`,{"role":"assistant","content":"done ` + filler + `"}`)
	}
	sb.WriteString(`]}`)
	return []byte(sb.String())
}

// extractMsgs 取出 body 的 messages。
func extractMsgs(t *testing.T, body []byte) []json.RawMessage {
	t.Helper()
	var obj struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return obj.Messages
}

// commonPrefixMsgs 计算两 body 的公共 messages 前缀条数（逐条原始 JSON
// 比对）。
func commonPrefixMsgs(t *testing.T, a, b []byte) int {
	ma, mb := extractMsgs(t, a), extractMsgs(t, b)
	n := 0
	for n < len(ma) && n < len(mb) && bytes.Equal(ma[n], mb[n]) {
		n++
	}
	return n
}

// TestCompactWithSummary_StableCutAcrossRounds 核心用例：
// 同一会话第 N 轮与第 N+1 轮（尾部追加）→ 出站前缀逐条字节一致
//（稳定裁剪点 + 摘要复用），仅尾部新增条目不同。
func TestCompactWithSummary_StableCutAcrossRounds(t *testing.T) {
	resetSummaryCache()
	lim := CompactLimits{MaxBodyBytes: 6000, KeepTail: 4}

	roundN := buildAgentTurns(10, strings.Repeat("x", 300))
	outN, folded := CompactBodyWithSummary(roundN, lim, "solo", "conv-stable")
	if outN == nil || folded == 0 {
		t.Fatalf("expected compaction with summary, got nil/0 (folded=%d)", folded)
	}
	if len(outN) > lim.MaxBodyBytes {
		t.Fatalf("out %d > budget %d", len(outN), lim.MaxBodyBytes)
	}

	roundN1 := buildAgentTurns(11, strings.Repeat("x", 300))
	outN1, folded1 := CompactBodyWithSummary(roundN1, lim, "solo", "conv-stable")
	if outN1 == nil || folded1 == 0 {
		t.Fatalf("expected second compaction, got nil/0")
	}

	common := commonPrefixMsgs(t, outN, outN1)
	msgsN := extractMsgs(t, outN)
	if common < len(msgsN) {
		t.Fatalf("prefix not stable across rounds: common=%d keptN=%d", common, len(msgsN))
	}
}

// TestCompactWithSummary_ChannelBuckets 分桶语义：
// 同一 conversationID 在不同通道（solo vs cn）各自独立缓存裁剪状态，
// 互不覆盖（两池 POST-PrepareBody 形态不同，指纹必然失配，分桶避免
// 反复重算与互相污染）。
func TestCompactWithSummary_ChannelBuckets(t *testing.T) {
	resetSummaryCache()
	lim := CompactLimits{MaxBodyBytes: 6000, KeepTail: 4}
	conv := "conv-bucket"

	// solo 池第 1 轮。
	srcSolo := buildAgentTurns(10, strings.Repeat("x", 300))
	outSolo1, _ := CompactBodyWithSummary(srcSolo, lim, "solo", conv)
	if outSolo1 == nil {
		t.Fatal("solo first compaction failed")
	}
	// cn 池同会话：不同通道桶，不命中 solo 的缓存，但自身正常产出。
	srcCN := buildAgentTurns(10, strings.Repeat("x", 300))
	outCN1, _ := CompactBodyWithSummary(srcCN, lim, "cn", conv)
	if outCN1 == nil {
		t.Fatal("cn first compaction failed")
	}
	// 两个桶现在各有一条 entry。
	summaryMu.Lock()
	_, hasSolo := summaryCache["solo|"+conv]
	_, hasCN := summaryCache["cn|"+conv]
	summaryMu.Unlock()
	if !hasSolo || !hasCN {
		t.Fatalf("expected separate channel buckets: solo=%v cn=%v", hasSolo, hasCN)
	}
	// solo 第 2 轮（同会话尾部追加）→ 命中 solo 桶缓存，前缀稳定。
	srcSolo2 := buildAgentTurns(11, strings.Repeat("x", 300))
	outSolo2, _ := CompactBodyWithSummary(srcSolo2, lim, "solo", conv)
	if outSolo2 == nil {
		t.Fatal("solo second compaction failed")
	}
	common := commonPrefixMsgs(t, outSolo1, outSolo2)
	msgs1 := extractMsgs(t, outSolo1)
	if common < len(msgs1) {
		t.Fatalf("solo bucket reuse must keep prefix stable: common=%d kept=%d", common, len(msgs1))
	}
}

// TestCompactWithSummary_SummaryShape：出站形态 = [system..., summary(user),
// msgs[cut:]...]；摘要含折叠条数与要点文本；保留序列首条不是 tool。
func TestCompactWithSummary_SummaryShape(t *testing.T) {
	resetSummaryCache()
	src := buildAgentTurns(12, strings.Repeat("y", 400)) // 49 条消息
	out, folded := CompactBodyWithSummary(src, CompactLimits{MaxBodyBytes: 6000, KeepTail: 4}, "solo", "conv-shape")
	if out == nil || folded == 0 {
		t.Fatalf("expected compaction, folded=%d", folded)
	}
	msgs := extractMsgs(t, out)
	if len(msgs) < 2 {
		t.Fatalf("too few messages: %d", len(msgs))
	}
	if msgRole(msgs[0]) != "system" {
		t.Fatalf("system prefix lost: %s", msgRole(msgs[0]))
	}
	if msgRole(msgs[1]) != "user" {
		t.Fatalf("summary must sit right after system prefix as user, got %s", msgRole(msgs[1]))
	}
	var sum struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(msgs[1], &sum); err != nil || len(sum.Content) == 0 {
		t.Fatalf("summary content malformed: %v", err)
	}
	if !strings.Contains(sum.Content[0].Text, "[会话历史压缩摘要]") {
		t.Fatalf("summary text missing marker: %q", sum.Content[0].Text)
	}
	if !strings.Contains(sum.Content[0].Text, "read_file") {
		t.Fatalf("summary should surface tool call name, got: %.200s", sum.Content[0].Text)
	}
	if msgRole(msgs[2]) == "tool" {
		t.Fatal("kept sequence must not start with an orphan tool result")
	}
}

// TestCompactWithSummary_CutNotOnToolMessage：
// 裁剪点落在 tool 消息上时会推进（保留序列首条非 tool）。
func TestCompactWithSummary_CutNotOnToolMessage(t *testing.T) {
	resetSummaryCache()
	var sb strings.Builder
	sb.WriteString(`{"model":"m","messages":[{"role":"system","content":"s"}`)
	for i := 0; i < 12; i++ {
		sb.WriteString(`,{"role":"user","content":"` + strings.Repeat("u", 400) + `"}`)
		sb.WriteString(`,{"role":"tool","content":"` + strings.Repeat("t", 400) + `"}`)
	}
	sb.WriteString(`,{"role":"assistant","content":"final"},{"role":"user","content":"q"}]}`)
	src := []byte(sb.String())

	out, folded := CompactBodyWithSummary(src, CompactLimits{MaxBodyBytes: 5000, KeepTail: 2}, "solo", "conv-toolcut")
	if out == nil || folded == 0 {
		t.Fatalf("expected compaction, folded=%d", folded)
	}
	msgs := extractMsgs(t, out)
	for _, m := range msgs[1:] {
		if strings.Contains(string(m), "会话历史压缩摘要") {
			continue
		}
		if msgRole(m) == "tool" {
			t.Fatal("cut landed on a tool message (orphan tool result at sequence head)")
		}
		break
	}
}

// TestCompactWithSummary_FingerprintMismatch：
// 客户端重写历史（msgs[cut] 内容变化）→ 指纹失配 → 重算新裁剪点。
func TestCompactWithSummary_FingerprintMismatch(t *testing.T) {
	resetSummaryCache()
	lim := CompactLimits{MaxBodyBytes: 6000, KeepTail: 4}
	conv := "conv-rewrite"

	round1 := buildAgentTurns(10, strings.Repeat("a", 300))
	out1, _ := CompactBodyWithSummary(round1, lim, "solo", conv)
	if out1 == nil {
		t.Fatal("first compaction failed")
	}
	round2 := buildAgentTurns(11, strings.Repeat("b", 300))
	out2, folded := CompactBodyWithSummary(round2, lim, "solo", conv)
	if out2 == nil {
		t.Fatal("second compaction failed after history rewrite")
	}
	if len(out2) > lim.MaxBodyBytes {
		t.Fatalf("out %d > budget", len(out2))
	}
	msgs := extractMsgs(t, out2)
	for _, m := range msgs[1:] {
		if strings.Contains(string(m), "会话历史压缩摘要") {
			continue
		}
		if msgRole(m) == "tool" {
			t.Fatal("orphan tool result at head after recompute")
		}
		break
	}
	if folded == 0 {
		t.Fatal("recompute must fold a non-empty mid-segment")
	}
}

// TestCompactWithSummary_Degrades：
// 无 conversationID / 预算内 / keepTail 禁用 → 返回 nil 交由调用方回落。
func TestCompactWithSummary_Degrades(t *testing.T) {
	resetSummaryCache()
	src := buildAgentTurns(6, strings.Repeat("c", 300))
	if out, _ := CompactBodyWithSummary(src, CompactLimits{MaxBodyBytes: 100}, "solo", ""); out != nil {
		t.Fatal("empty conversationID must return nil (fallback path)")
	}
	if out, _ := CompactBodyWithSummary(src, CompactLimits{MaxBodyBytes: 1 << 20}, "solo", "conv-deg"); out != nil {
		t.Fatal("under-budget must return nil")
	}
	if out, _ := CompactBodyWithSummary(src, CompactLimits{MaxBodyBytes: 100, KeepTail: -1}, "solo", "conv-deg"); out != nil {
		t.Fatal("disabled compaction must return nil")
	}
	if out, _ := CompactBodyWithSummary([]byte(`{bad`), CompactLimits{MaxBodyBytes: 5}, "solo", "conv-deg"); out != nil {
		t.Fatal("invalid JSON must return nil")
	}
}

// TestCompactWithSummary_GiantTailFallsBack：
// 尾段单轮巨消息（保底尾段即超预算）→ 返回 nil 回落旧 CompactBody
// 最小形态路径，不得死循环/不得产出超预算结果。
func TestCompactWithSummary_GiantTailFallsBack(t *testing.T) {
	resetSummaryCache()
	var sb strings.Builder
	sb.WriteString(`{"model":"m","messages":[{"role":"system","content":"s"}`)
	for i := 0; i < 10; i++ {
		sb.WriteString(`,{"role":"user","content":"small"}`)
	}
	for i := 0; i < 2; i++ {
		sb.WriteString(`,{"role":"user","content":"` + strings.Repeat("G", 8000) + `"}`)
	}
	sb.WriteString(`]}`)
	src := []byte(sb.String())
	out, folded := CompactBodyWithSummary(src, CompactLimits{MaxBodyBytes: 5000, KeepTail: 4}, "solo", "conv-giant")
	if out != nil || folded != 0 {
		t.Fatalf("giant tail must fall back (nil), got out=%d folded=%d", len(out), folded)
	}
}

// TestCompactWithSummary_ExpiryAndEviction：TTL 过期后重算；缓存容量上限
// 触发惰性清理不 panic。
func TestCompactWithSummary_ExpiryAndEviction(t *testing.T) {
	resetSummaryCache()
	lim := CompactLimits{MaxBodyBytes: 6000, KeepTail: 4}
	conv := "conv-ttl"

	src := buildAgentTurns(10, strings.Repeat("d", 300))
	if out, _ := CompactBodyWithSummary(src, lim, "solo", conv); out == nil {
		t.Fatal("first compaction failed")
	}
	// 手动过期。
	summaryMu.Lock()
	if e := summaryCache["solo|"+conv]; e != nil {
		e.expiresAt = time.Now().Add(-time.Second)
	}
	// 容量打满触发清理路径。
	for i := 0; i < summaryCacheMax+16; i++ {
		summaryCache["filler-"+string(rune('a'+i%26))+time.Now().Format("150405.000000000")+string(rune('A'+i%26))] = &summaryCacheEntry{
			cutIndex: 1, firstKeptFP: "x", summary: json.RawMessage(`{}`),
			droppedCount: 1, expiresAt: time.Now().Add(time.Hour),
		}
	}
	summaryMu.Unlock()

	out, folded := CompactBodyWithSummary(src, lim, "solo", conv)
	if out == nil || folded == 0 {
		t.Fatalf("recompute after expiry/eviction failed: folded=%d", folded)
	}
	if len(out) > lim.MaxBodyBytes {
		t.Fatalf("out %d > budget", len(out))
	}
}

// TestCompactWithSummary_ConcurrentSameConv：
// 同会话并发请求不竞态（mutex 保护）、结果一致。
func TestCompactWithSummary_ConcurrentSameConv(t *testing.T) {
	resetSummaryCache()
	lim := CompactLimits{MaxBodyBytes: 6000, KeepTail: 4}
	conv := "conv-race"
	src := buildAgentTurns(10, strings.Repeat("e", 300))

	var wg sync.WaitGroup
	outs := make([][]byte, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, _ := CompactBodyWithSummary(src, lim, "solo", conv)
			outs[i] = out
		}(i)
	}
	wg.Wait()
	for i, out := range outs {
		if out == nil {
			t.Fatalf("worker %d got nil", i)
		}
		if !bytes.Equal(out, outs[0]) {
			t.Fatalf("worker %d output diverged", i)
		}
	}
}

// TestEndToEnd_CacheKeySurvivesCompaction（端到端管线断言）：
// InjectPromptCacheKey → CompactBody 全链路后 key 仍在（reassemble 保留
// 顶层字段），且瘦身路径不吞掉它。
func TestEndToEnd_CacheKeySurvivesCompaction(t *testing.T) {
	msgs := `[{"role":"system","content":"sys-a"},{"role":"system","content":"sys-b"}`
	filler := strings.Repeat("x", 2000)
	for i := 0; i < 40; i++ {
		role := "user"
		if i%3 == 1 {
			role = "assistant"
		} else if i%3 == 2 {
			role = "tool"
		}
		msgs += `,{"role":"` + role + `","content":"` + filler + `"}`
	}
	msgs += "]"
	in := []byte(`{"model":"glm-5.2","conversation_id":"conv-big","messages":` + msgs + `}`)

	keyed := InjectPromptCacheKey(in, "")
	if got := extractCacheKey(t, keyed); !strings.HasPrefix(got, "wb2a-") {
		t.Fatalf("expected injected key, got=%q", got)
	}
	compacted, dropped := CompactBody(keyed, CompactLimits{MaxBodyBytes: 1 << 16, KeepTail: 4})
	if dropped == 0 {
		t.Fatalf("expected compaction to drop messages")
	}
	if got := extractCacheKey(t, compacted); !strings.HasPrefix(got, "wb2a-") {
		t.Fatalf("prompt_cache_key must survive compaction, got=%q", got)
	}
}
