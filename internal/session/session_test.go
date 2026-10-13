package session

import (
	"strings"
	"testing"
)

func TestNoSessionKeyPassthrough(t *testing.T) {
	// ExtractKey 找不到任何会话键 → 空串（调用方据空串回落轮级键）。
	got := ExtractKey([]byte(`{"model":"x","messages":[]}`))
	if got != "" {
		t.Errorf("ExtractKey should return empty, got %q", got)
	}
}

func TestExtractKeyPriority(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"metadata":{"conversation_id":"mc","user_id":"mu"},"conversation_id":"top"}`, "mc"}, // metadata.conversation_id 优先
		{`{"conversation_id":"top"}`, "top"},                                                   // 顶层 conversation_id
		{`{"metadata":{"user_id":"mu"}}`, ""},                                                  // user_id 已剔除（粒度过粗），回落派生/轮级
		{`{"metadata":{"conversation_id":123}}`, ""},                                           // 非字符串 → 空
		{`not-json`, ""}, // 非法 JSON → 空
		// issue #35：客户端实际发 camelCase conversationId，ExtractKey 必须识别。
		{`{"conversationId":"abc"}`, "abc"},                                            // 顶层 camelCase
		{`{"metadata":{"conversationId":"abc"}}`, "abc"},                               // metadata.camelCase
		{`{"metadata":{"conversation_id":"snake","conversationId":"camel"}}`, "snake"}, // snake 优先于 camel
		{`{"conversation_id":"snake","conversationId":"camel"}`, "snake"},              // 顶层 snake 优先于 camel
		{`{"conversationId":123}`, ""},                                                 // 数字 conversationId → 空
		{`{"metadata":{"conversationId":456}}`, ""},                                    // metadata 数字 conversationId → 空
		{`{"metadata":{"conversationId":"abc","user_id":"mu"}}`, "abc"},                // camel conversationId 优先于 user_id
	}
	for _, c := range cases {
		if got := ExtractKey([]byte(c.body)); got != c.want {
			t.Errorf("ExtractKey(%s)=%q want %q", c.body, got, c.want)
		}
	}
}

// TestExtractKeyDerivedFromContent 客户端不发会话 id 时回退到内容派生键：
// 同一对话多轮（历史追加）→ 键稳定不变；不同对话 → 键不同。
func TestExtractKeyDerivedFromContent(t *testing.T) {
	// 第一轮
	turn1 := `{"model":"glm-5.3","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"帮我写个排序算法"}]}`
	// 第二轮：历史追加了 assistant 与新的 user（system 与首条 user 不变）
	turn2 := `{"model":"glm-5.3","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"帮我写个排序算法"},{"role":"assistant","content":"好的"},{"role":"user","content":"换成快排"}]}`
	k1, k2 := ExtractKey([]byte(turn1)), ExtractKey([]byte(turn2))
	if k1 == "" {
		t.Fatal("derived key should not be empty when messages present")
	}
	if k1 != k2 {
		t.Errorf("derived key must be stable across turns: turn1=%q turn2=%q", k1, k2)
	}
	if !strings.HasPrefix(k1, "d-") {
		t.Errorf("derived key should carry prefix d-: %q", k1)
	}
	// 不同对话（首条 user 不同）→ 不同键
	other := `{"model":"glm-5.3","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"翻译这段话"}]}`
	if ExtractKey([]byte(other)) == k1 {
		t.Error("different first user message must yield a different derived key")
	}
	// 显式 id 优先于派生键
	withID := `{"conversation_id":"my-session","messages":[{"role":"user","content":"帮我写个排序算法"}]}`
	if got := ExtractKey([]byte(withID)); got != "my-session" {
		t.Errorf("explicit id must win over derived key, got %q", got)
	}
	// 无 messages / 纯无文本内容 → 空（退回轮级键）
	if got := ExtractKey([]byte(`{"model":"x"}`)); got != "" {
		t.Errorf("no messages should yield empty key, got %q", got)
	}
	if got := ExtractKey([]byte(`{"messages":[{"role":"user","content":[]}]}`)); got != "" {
		t.Errorf("text-less content should yield empty key, got %q", got)
	}
}

// TestExtractKeyMultimodalContent 多模态 content 数组取文本部分派生。
func TestExtractKeyMultimodalContent(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"text","text":"看图说话"},{"type":"image_url","image_url":{"url":"http://x/y.png"}}]}]}`
	k := ExtractKey([]byte(body))
	if k == "" || !strings.HasPrefix(k, "d-") {
		t.Fatalf("multimodal text should derive a key, got %q", k)
	}
	// 同一文本（图片不同）→ 同键（图片不参与派生，避免签名 URL 变化破坏键稳定性）
	body2 := `{"messages":[{"role":"user","content":[{"type":"text","text":"看图说话"},{"type":"image_url","image_url":{"url":"http://x/z.png"}}]}]}`
	if ExtractKey([]byte(body2)) != k {
		t.Error("image url changes must not break derived key stability")
	}
	// 纯图片首条 user（无 text part）也应派生非空键（图片只入类型占位，同图重发/换 URL 均同键）。
	imgOnly := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://x/y.png"}}]}]}`
	k2 := ExtractKey([]byte(imgOnly))
	if k2 == "" || !strings.HasPrefix(k2, "d-") {
		t.Fatalf("pure-image first user should derive a key, got %q", k2)
	}
	imgOnly2 := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://x/z.png"}}]}]}`
	if ExtractKey([]byte(imgOnly2)) != k2 {
		t.Error("pure-image url changes must keep same derived key")
	}
}
