package promptcache

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// extractCacheKey 从出站 body 里取出 prompt_cache_key 字段值。
func extractCacheKey(t *testing.T, body []byte) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	v, _ := obj["prompt_cache_key"].(string)
	return v
}

// TestBuildCacheKey_CrossPoolStable 跨池/跨账号一致性（本合并的核心目标）：
// cache key 只由会话标识决定——同一会话无论池 A 账号 1 还是池 B 账号 2
// 发起（body 的 model/messages 随账号工具形态变化也无妨），key 恒定；
// 键中不含任何账号段。
func TestBuildCacheKey_CrossPoolStable(t *testing.T) {
	ka := BuildCacheKey("conv-shared")
	kb := BuildCacheKey("conv-shared")
	if ka == "" || ka != kb {
		t.Fatalf("key must be deterministic per conversation: %q vs %q", ka, kb)
	}
	// 不同会话必须不同。
	if BuildCacheKey("conv-other") == ka {
		t.Fatalf("different conversation produced same key: %q", ka)
	}
	// 格式：wb2a- + 32 hex（无账号段）。
	if !strings.HasPrefix(ka, "wb2a-") || len(ka) != len("wb2a-")+32 {
		t.Fatalf("unexpected key format: %q", ka)
	}
}

// TestBuildCacheKey_Format：wb2a-<convHex>，convHex 是 hex 字符。
func TestBuildCacheKey_Format(t *testing.T) {
	got := BuildCacheKey("conv-fmt")
	if !strings.HasPrefix(got, "wb2a-") {
		t.Fatalf("expected wb2a- prefix, got=%q", got)
	}
	hexPart := strings.TrimPrefix(got, "wb2a-")
	for _, c := range hexPart {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("non-hex char in key: %q", got)
		}
	}
}

// TestInjectPromptCacheKey_PreservesExisting：
// body 已带 prompt_cache_key → 原值保留，不被覆盖（客户端自知复用哪个键）。
func TestInjectPromptCacheKey_PreservesExisting(t *testing.T) {
	in := `{"model":"glm-5.2","messages":[],"prompt_cache_key":"client-set-key"}`
	out := InjectPromptCacheKey([]byte(in), "conv-xyz")
	if got := extractCacheKey(t, out); got != "client-set-key" {
		t.Fatalf("existing prompt_cache_key overwritten: got=%q", got)
	}
}

// TestInjectPromptCacheKey_UsesConversationID：
// body 无 key 但有 conversation_id → 用它做会话哈希源（同 id 同 key、
// 不同 id 不同 key）。
func TestInjectPromptCacheKey_UsesConversationID(t *testing.T) {
	in := `{"model":"glm-5.2","messages":[],"conversation_id":"conv-42"}`
	out := InjectPromptCacheKey([]byte(in), "")
	got := extractCacheKey(t, out)
	if got != BuildCacheKey("conv-42") {
		t.Fatalf("expected key derived from body conversation_id, got=%q", got)
	}
	if got2 := extractCacheKey(t, InjectPromptCacheKey([]byte(in), "")); got2 != got {
		t.Fatalf("non-deterministic key for same conversation_id: %q vs %q", got, got2)
	}
	inB := `{"model":"glm-5.2","messages":[],"conversationId":"conv-99"}`
	if gotB := extractCacheKey(t, InjectPromptCacheKey([]byte(inB), "")); gotB == got {
		t.Fatalf("different conversation_id produced same key: %q", got)
	}
}

// TestInjectPromptCacheKey_ParamFallback：
// body 无会话字段 → 入参 conversationID 兜底（SOLO 主路径：PrepareBody
// 已剔除 body 内 conversation_id，会话源经参数传入）。
func TestInjectPromptCacheKey_ParamFallback(t *testing.T) {
	in := `{"model":"glm-5.2","messages":[]}`
	ka := extractCacheKey(t, InjectPromptCacheKey([]byte(in), "conv-stable"))
	kb := extractCacheKey(t, InjectPromptCacheKey([]byte(in), "conv-stable"))
	if ka == "" || ka != kb {
		t.Fatalf("non-deterministic or empty key: a=%q b=%q", ka, kb)
	}
	if kc := extractCacheKey(t, InjectPromptCacheKey([]byte(in), "conv-other")); kc == ka {
		t.Fatalf("different param conversation produced same key: %q", ka)
	}
}

// TestInjectPromptCacheKey_EmptyConversation：
// 无会话源 → 仍注入非空 key（全常量；统一解析链让空源罕见）。
func TestInjectPromptCacheKey_EmptyConversation(t *testing.T) {
	out := InjectPromptCacheKey([]byte(`{"model":"glm-5.2","messages":[]}`), "")
	got := extractCacheKey(t, out)
	if got == "" || got != BuildCacheKey("") {
		t.Fatalf("expected constant key for empty conversation, got=%q", got)
	}
}

// TestInjectPromptCacheKey_InvalidJSON：坏 body 原样返回，不二次错误化。
func TestInjectPromptCacheKey_InvalidJSON(t *testing.T) {
	in := []byte(`{not json`)
	if out := InjectPromptCacheKey(in, "c1"); string(out) != string(in) {
		t.Fatalf("invalid JSON body must pass through unchanged, got %q", out)
	}
}

// TestResolveConversation 统一会话解析链优先级：
// metadata → 顶层 → 头族 → user → 指纹。
func TestResolveConversation(t *testing.T) {
	// 1+2. body 四级。
	if got := ResolveConversation(nil, []byte(`{"metadata":{"conversation_id":"m-1"},"messages":[]}`)); got != "m-1" {
		t.Fatalf("metadata.conversation_id: got=%q", got)
	}
	if got := ResolveConversation(nil, []byte(`{"conversationId":"t-1","messages":[]}`)); got != "t-1" {
		t.Fatalf("top-level conversationId: got=%q", got)
	}
	// 3. 头族。
	h := http.Header{}
	h.Set("X-Conversation-ID", "hdr-1")
	if got := ResolveConversation(h, []byte(`{"messages":[]}`)); got != "hdr-1" {
		t.Fatalf("header: got=%q", got)
	}
	h2 := http.Header{}
	h2.Set("Session-Id", "sess-1")
	if got := ResolveConversation(h2, []byte(`{"messages":[]}`)); got != "sess-1" {
		t.Fatalf("Session-Id header: got=%q", got)
	}
	// 4. user 字段。
	if got := ResolveConversation(nil, []byte(`{"user":"u-1","messages":[]}`)); got != "u-1" {
		t.Fatalf("user field: got=%q", got)
	}
	// 5. 指纹：同一多轮会话追加尾部消息 → 指纹稳定；不同首条消息 → 不同。
	bodyA := []byte(`{"messages":[{"role":"system","content":"s"},{"role":"user","content":"q1"},{"role":"assistant","content":"a1"}]}`)
	bodyA2 := []byte(`{"messages":[{"role":"system","content":"s"},{"role":"user","content":"q1"},{"role":"assistant","content":"a1"},{"role":"user","content":"q2"}]}`)
	bodyB := []byte(`{"messages":[{"role":"system","content":"s"},{"role":"user","content":"OTHER"}]}`)
	fa, fa2, fb := ResolveConversation(nil, bodyA), ResolveConversation(nil, bodyA2), ResolveConversation(nil, bodyB)
	if !strings.HasPrefix(fa, "fp:") || fa != fa2 {
		t.Fatalf("fingerprint must be prefix-stable: %q vs %q", fa, fa2)
	}
	if fa == fb {
		t.Fatalf("different first turn must produce different fingerprint: %q", fa)
	}
	// 全源缺失。
	if got := ResolveConversation(nil, []byte(`{"messages":[]}`)); got != "" {
		t.Fatalf("no source must return empty, got=%q", got)
	}
	if got := ResolveConversation(nil, []byte(`{bad`)); got != "" {
		t.Fatalf("invalid JSON must return empty, got=%q", got)
	}
}
