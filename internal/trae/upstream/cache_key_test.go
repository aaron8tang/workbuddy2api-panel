package upstream

import (
	"encoding/json"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/promptcache"
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

// TestSoloPrepareBodyKeepsClientCacheKey：
// 白名单放行 prompt_cache_key → 客户端自带 key 经 PrepareBody 存活，
// 随后 promptcache.InjectPromptCacheKey 不覆盖。
// （注入/稳定性/跨账号跨池一致性的用例已上移共享包 internal/promptcache。）
func TestSoloPrepareBodyKeepsClientCacheKey(t *testing.T) {
	in := `{"model":"glm-5.2","messages":[],"prompt_cache_key":"client-key","destination_addr":"evil"}`
	prepared := PrepareBody([]byte(in))
	var obj map[string]any
	if err := json.Unmarshal(prepared, &obj); err != nil {
		t.Fatalf("unmarshal prepared: %v", err)
	}
	if v, _ := obj["prompt_cache_key"].(string); v != "client-key" {
		t.Fatalf("client prompt_cache_key must survive PrepareBody, got %v", obj["prompt_cache_key"])
	}
	if _, present := obj["destination_addr"]; present {
		t.Fatalf("non-whitelisted field must be stripped")
	}
	out := promptcache.InjectPromptCacheKey(prepared, "c1")
	if got := extractCacheKey(t, out); got != "client-key" {
		t.Fatalf("existing key must not be overwritten, got=%q", got)
	}
}
