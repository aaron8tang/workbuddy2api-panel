package qwenwork

import (
	"encoding/json"
	"testing"
	"time"
)

func TestParseCredAuthV2Shape(t *testing.T) {
	// 对齐桌面端解密 auth-v2.dat 的真实形态
	raw := []byte(`{
		"schemaVersion": 2,
		"token": "eyJhbGciOiJFUzI1NiJ9.eyJuYW1lIjoidTEifQ.sig",
		"refreshToken": "ory_rt_abc",
		"expiresAt": "2026-10-14T23:14:55Z",
		"refreshTokenExpiresAt": "2026-11-08T00:14:52.856Z",
		"user": {"id": "bd5f59ff-1111-2222-3333-444455556666", "name": "cookman", "email": "c@x.com", "username": "01071tr"},
		"loginDeviceId": "d7b9cc8e-8823-4056-bde7-3022c1bbc408",
		"loginMethod": "browser",
		"refreshStrategy": "device_token"
	}`)
	c, err := ParseCred(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.UID != "bd5f59ff-1111-2222-3333-444455556666" || c.Name != "cookman" {
		t.Errorf("identity mismatch: %+v", c)
	}
	if c.RefreshToken != "ory_rt_abc" {
		t.Errorf("refresh=%q", c.RefreshToken)
	}
	if !c.ExpiresAt.Equal(time.Date(2026, 10, 14, 23, 14, 55, 0, time.UTC)) {
		t.Errorf("expiresAt=%v", c.ExpiresAt)
	}
	if !c.HasCredential() {
		t.Error("should have credential")
	}
}

func TestCredRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := &Cred{
		UID: "u1", Name: "n", Token: "tok", RefreshToken: "rt",
		ExpiresAt: time.Unix(1900000000, 0), MachineID: "mid",
		RuntimeKey: "rk", RuntimeInfo: "ri",
	}
	if err := c.Save(dir); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadFile(c.FilePath(dir))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Token != "tok" || got.MachineID != "mid" || got.RuntimeKey != "rk" || got.RuntimeInfo != "ri" {
		t.Errorf("mismatch: %+v", got)
	}
}

func TestBuildAgentBodyQwenWork(t *testing.T) {
	msgs := []map[string]any{{"role": "user", "content": "你好，请只回复两个字"}}
	raw, err := BuildAgentBody(msgs, "flash", nil, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	// CN CLI 口径：session_type=qodercli；JWT 身份无 aliyun_user_type（不发）
	if m["session_type"] != "qodercli" {
		t.Errorf("session_type=%v", m["session_type"])
	}
	if _, ok := m["aliyun_user_type"]; ok {
		t.Error("aliyun_user_type must be absent for qwenwork")
	}
	if m["model_config"].(map[string]any)["key"] != "flash" {
		t.Errorf("model_config=%v", m["model_config"])
	}
}

func TestBuildAgentBodyBusinessNameTruncated(t *testing.T) {
	long := "这是一条非常长的用户消息需要被截断到三十个字符以内以免上游会话名超长显示异常"
	raw, _ := BuildAgentBody([]map[string]any{{"role": "user", "content": long}}, "flash", nil, "")
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	name := m["business"].(map[string]any)["name"].(string)
	if len([]rune(name)) > 30 {
		t.Errorf("business.name must be <=30 runes, got %d", len([]rune(name)))
	}
}
