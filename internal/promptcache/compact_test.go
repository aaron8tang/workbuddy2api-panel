// compact_test.go 出站请求体历史瘦身测试：预算内零改动 / 超预算裁中段 /
// system 前缀与尾段保留 / 单条巨消息降级处理。
package promptcache

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// buildBody 构造 n 条消息的 body；第 i 条内容带填充以控制大小。
func buildBody(n int, filler string, systemPrefix int) []byte {
	var sb strings.Builder
	sb.WriteString(`{"model":"glm-5.3","messages":[`)
	first := true
	add := func(role string) {
		if !first {
			sb.WriteString(",")
		}
		first = false
		sb.WriteString(`{"role":"` + role + `","content":"` + filler + `"}`)
	}
	for i := 0; i < systemPrefix; i++ {
		add("system")
	}
	for i := 0; i < n; i++ {
		switch i % 3 {
		case 0:
			add("user")
		case 1:
			add("assistant")
		default:
			add("tool")
		}
	}
	sb.WriteString(`],"stream":true}`)
	return []byte(sb.String())
}

func TestCompactBodyUnderBudget(t *testing.T) {
	// 预算内：零改动（字节级一致）。
	src := buildBody(10, "x", 1)
	out, dropped := CompactBody(src, CompactLimits{MaxBodyBytes: len(src) + 1})
	if dropped != 0 || !bytes.Equal(src, out) {
		t.Fatalf("under-budget body must be untouched: dropped=%d equal=%v", dropped, bytes.Equal(src, out))
	}
}

func TestCompactBodyDropsOldestMid(t *testing.T) {
	// 20 条 user 消息（非均匀大小：后面的更大），预算收紧到必须裁剪。
	var sb strings.Builder
	sb.WriteString(`{"model":"m","messages":[`)
	for i := 0; i < 20; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fill := "x"
		if i >= 10 {
			fill = strings.Repeat("y", 500)
		}
		sb.WriteString(`{"role":"user","content":"` + fill + `"}`)
	}
	sb.WriteString(`]}`)
	src := []byte(sb.String())

	out, dropped := CompactBody(src, CompactLimits{MaxBodyBytes: len(src) / 2, KeepTail: 4})
	if dropped == 0 {
		t.Fatal("expected dropping, got 0")
	}
	if len(out) > len(src)/2 {
		t.Fatalf("result %d bytes > budget %d", len(out), len(src)/2)
	}
	if !strings.Contains(string(out), "yyy") {
		t.Fatal("tail messages (large) were dropped")
	}
	var obj struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if len(obj.Messages) < 5 {
		t.Fatalf("kept too few messages: %d", len(obj.Messages))
	}
	if strings.Count(string(out), `"content":"x"`) > 6 {
		t.Fatalf("oldest small messages not dropped: %d", strings.Count(string(out), `"content":"x"`))
	}
}

func TestCompactBodyKeepsSystemAndTail(t *testing.T) {
	// system 前缀 + 大量中段 + 尾段；验证 system 保留、尾段条数保底。
	src := buildBody(30, strings.Repeat("z", 300), 2)
	out, dropped := CompactBody(src, CompactLimits{MaxBodyBytes: 4000, KeepTail: 3})
	if dropped == 0 {
		t.Fatal("expected dropping")
	}
	var obj struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if len(obj.Messages) == 0 {
		t.Fatal("empty messages")
	}
	if obj.Messages[0].Role != "system" || obj.Messages[1].Role != "system" {
		t.Fatalf("system prefix lost: %v", obj.Messages[0].Role)
	}
	if len(obj.Messages) < 3+2 {
		t.Fatalf("tail keep violated: %d", len(obj.Messages))
	}
}

func TestCompactBodyTailSwallowsAll(t *testing.T) {
	// keepTail ≥ 消息数：不裁。
	src := buildBody(5, "q", 1)
	out, dropped := CompactBody(src, CompactLimits{MaxBodyBytes: 10, KeepTail: 10})
	if dropped != 0 {
		t.Fatalf("keepTail >= len must not drop: %d", dropped)
	}
	if !bytes.Equal(src, out) {
		t.Fatal("must return original")
	}
}

func TestCompactBodyInvalidJSON(t *testing.T) {
	src := []byte(`{not json`)
	out, dropped := CompactBody(src, CompactLimits{MaxBodyBytes: 5})
	if dropped != 0 || !bytes.Equal(src, out) {
		t.Fatal("invalid json must pass through unchanged")
	}
}

func TestCompactBodyDisabled(t *testing.T) {
	src := buildBody(10, strings.Repeat("a", 400), 1)
	out, dropped := CompactBody(src, CompactLimits{MaxBodyBytes: 100, KeepTail: -1})
	if dropped != 0 || !bytes.Equal(src, out) {
		t.Fatal("KeepTail<0 must disable compaction")
	}
}
