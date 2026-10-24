package qoderwork

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeReasoningEffort(t *testing.T) {
	cases := map[string]string{
		"low": "low", "LOW": "low", " medium ": "medium", "xhigh": "xhigh",
		"high": "", "none": "", "": "", "max": "",
	}
	for in, want := range cases {
		if got := NormalizeReasoningEffort(in); got != want {
			t.Errorf("NormalizeReasoningEffort(%q)=%q want %q", in, got, want)
		}
	}
}

func TestBuildAgentBodyShape(t *testing.T) {
	msgs := []map[string]any{
		{"role": "system", "content": "you are helpful"},
		{"role": "user", "content": "你好"},
	}
	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "f"}}}
	raw, err := BuildAgentBody(msgs, "qmodel_preview", tools, "xhigh")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"request_id", "chat_record_id", "request_set_id", "session_id",
		"stream", "agent_id", "chat_task", "session_type", "model_config", "chat_context", "messages", "business"} {
		if _, ok := m[key]; !ok {
			t.Errorf("missing field %q", key)
		}
	}
	if m["session_type"] != "qodercli" {
		t.Errorf("session_type=%v", m["session_type"])
	}
	if m["stream"] != true {
		t.Errorf("stream=%v", m["stream"])
	}
	// tools 注入
	if _, ok := m["tools"]; !ok {
		t.Error("tools missing when provided")
	}
	// parameters 注入
	params, ok := m["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters missing: %v", m["parameters"])
	}
	if params["reasoning_effort"] != "xhigh" || params["enable_thinking"] != true {
		t.Errorf("parameters=%v", params)
	}
	// messages 原样透传
	if len(m["messages"].([]any)) != 2 {
		t.Errorf("messages=%v", m["messages"])
	}
}

func TestBuildAgentBodyOmitsOptionalFields(t *testing.T) {
	raw, err := BuildAgentBody([]map[string]any{{"role": "user", "content": "hi"}}, "k", nil, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	s := string(raw)
	if strings.Contains(s, "tools") {
		t.Error("tools must be omitted when client sends none")
	}
	if strings.Contains(s, "parameters") {
		t.Error("parameters must be omitted when no effort")
	}
}

func TestNormalizeModelName(t *testing.T) {
	cases := map[string]string{
		"Qwen3.7-Max":        "qwen3.7-max",
		"DeepSeek_V4 Pro":    "deepseek-v4-pro",
		"Kimi K2.7 Code":     "kimi-k2.7-code",
		"  GLM--5.2  ":       "glm-5.2",
		"千问 Max":             "千问-max",
	}
	for in, want := range cases {
		if got := NormalizeModelName(in); got != want {
			t.Errorf("NormalizeModelName(%q)=%q want %q", in, got, want)
		}
	}
}

func TestCredRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := &Cred{UID: "u1", Nickname: "nick", DT: "dt-x", DRT: "drt-y", DTExpiresAt: 1900000000}
	if err := c.Save(dir); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadFile(c.FilePath(dir))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.UID != "u1" || got.DT != "dt-x" || got.DRT != "drt-y" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

func TestLoadDirSkipsBrokenFiles(t *testing.T) {
	dir := t.TempDir()
	if err := (&Cred{UID: "ok1", DT: "dt", DRT: "drt"}).Save(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "qoderwork-bad.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	creds, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("loaddir: %v", err)
	}
	if len(creds) != 1 || creds[0].UID != "ok1" {
		t.Errorf("creds=%v", creds)
	}
}
