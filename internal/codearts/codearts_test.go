package codearts

import (
	"encoding/json"
	"testing"
)

func TestPrepareBodyThinkingOffDropped(t *testing.T) {
	body := map[string]any{
		"model":           "client-name",
		"stream":          false,
		"reasoning_effort": "none",
		"enable_thinking":  false,
		"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
	}
	prepareBody(body, "GLM-5.2", true)
	if _, has := body["reasoning_effort"]; has {
		t.Error("reasoning_effort=none 应被摘掉（本家没有关闭思考的档）")
	}
	if _, has := body["enable_thinking"]; has {
		t.Error("enable_thinking=false 应被摘掉")
	}
	if body["model"] != "GLM-5.2" {
		t.Errorf("model 应强制写上游真名: %v", body["model"])
	}
	if body["stream"] != true {
		t.Error("stream 应强制写")
	}
	so, ok := body["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Error("流式应注入 stream_options.include_usage")
	}
	// low 档不摘。
	body2 := map[string]any{"reasoning_effort": "low"}
	prepareBody(body2, "m", false)
	if body2["reasoning_effort"] != "low" {
		t.Error("low 档应保留")
	}
}

func TestPrepareBodyClampAndToolChoice(t *testing.T) {
	body := map[string]any{
		"max_tokens":  float64(128000),
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "read"}},
	}
	prepareBody(body, "m", false)
	if body["max_tokens"] != float64(MaxOutputTokens) {
		t.Errorf("max_tokens 应钳到 65536: %v", body["max_tokens"])
	}
	if body["tool_choice"] != "required" {
		t.Errorf("tool_choice 对象形态应折成 required: %v", body["tool_choice"])
	}
}

func TestStripHistoricalReasoning(t *testing.T) {
	body := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "q"},
		map[string]any{"role": "assistant", "content": "a", "reasoning_content": "thinking..."},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "thinking", "text": "hidden"},
			map[string]any{"type": "text", "text": "visible"},
		}},
	}}
	stripHistoricalReasoning(body)
	msgs := body["messages"].([]any)
	if _, has := msgs[1].(map[string]any)["reasoning_content"]; has {
		t.Error("assistant reasoning_content 应被丢掉")
	}
	parts := msgs[2].(map[string]any)["content"].([]any)
	if len(parts) != 1 {
		t.Errorf("thinking 块应被丢掉，剩 1 块: %d", len(parts))
	}
}

func TestFrameFaultQuota(t *testing.T) {
	frame := `{"error_code":"InferHub.4291.200","error_msg":"insufficient quota","details":[{"error_msg":"requestId: abe754a5417344caa306d5b38a57923b"}]}`
	has, fault := frameFault(frame)
	if has || fault == nil {
		t.Fatalf("额度信封应判故障: has=%v fault=%v", has, fault)
	}
	if fault.Status != 403 {
		t.Errorf("insufficient quota 应折 403: %d", fault.Status)
	}
	// details 含冒号（requestId:）不进消息。
	if len(fault.Message) > 0 && contains(fault.Message, "requestId") {
		t.Errorf("details 的 trace 装饰不应进消息: %s", fault.Message)
	}
}

func TestFrameFaultAnswerWins(t *testing.T) {
	// 带错误码又带内容的帧：有答案就不判故障（丢内容比漏报故障更糟）。
	frame := `{"error_code":"X","error_msg":"oops","choices":[{"delta":{"content":"hello"}}]}`
	has, fault := frameFault(frame)
	if !has || fault != nil {
		t.Errorf("带答案的帧不应判故障: has=%v fault=%v", has, fault)
	}
}

func TestFrameFaultSuccessCodes(t *testing.T) {
	for _, code := range []string{"", "0", "0000", "success"} {
		payload := `{"error_code":"` + code + `"}`
		has, fault := frameFault(payload)
		if !has || fault != nil {
			t.Errorf("成功码 %q 不应判故障: has=%v fault=%v", code, has, fault)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestParseBuiltin(t *testing.T) {
	raw := []byte(`{"count":2,"builtinModels":[
		{"model_id":"GLM-5.2","model_name":"GLM-5.2","enable":true,"display_enabled":true,
		 "context_window":202752,"max_tokens":131072,
		 "credit":[{"ratio_display":"0.7x"}]},
		{"model_id":"Hidden","enable":false}
	]}`)
	models, err := parseBuiltin(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 {
		t.Fatalf("enable=false 应跳过: %d", len(models))
	}
	m := models[0]
	if m.ID != "GLM-5.2" || m.CreditDisplay != "0.7x" {
		t.Errorf("字段映射错误: %+v", m)
	}
	if m.MaxOutputTokens != MaxOutputTokens {
		t.Errorf("max_tokens 131072 应钳到 65536: %d", m.MaxOutputTokens)
	}
}

func TestParseBenefitEnvelope(t *testing.T) {
	raw := []byte(`{"error_code":"0000","result":{"models":[
		{"model_id":"glm-5.3-flash","sort":20},
		{"model_id":"deepseek-v4-flash","sort":10}
	]}}`)
	models, err := parseBenefit(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatal(len(models))
	}
	// sort 升序：deepseek(10) 在 glm(20) 前。
	if models[0].ID != "deepseek-v4-flash" {
		t.Errorf("应按 sort 升序: %s first", models[0].ID)
	}
	if models[0].Source != SourceBenefit {
		t.Error("source 应为 benefit")
	}
	// error_code != 0000 报错。
	if _, err := parseBenefit([]byte(`{"error_code":"4004"}`)); err == nil {
		t.Error("envelope 失败应报错")
	}
}

func TestCatalogResolveCaseInsensitive(t *testing.T) {
	cat := &Catalog{Models: []ModelConfig{{ID: "GLM-5.2", Source: SourceAgent}}}
	m, ok := cat.Resolve("glm-5.2")
	if !ok || m.ID != "GLM-5.2" {
		t.Errorf("大小写无关解析失败: ok=%v m=%+v", ok, m)
	}
}

func TestParseBenefitBalanceAbsent(t *testing.T) {
	// 4004 + benefit not found = 正常状态（nil, nil）。
	b, err := parseBenefitBalance([]byte(`{"error_code":"4004","error_msg":"benefit not found"}`))
	if err != nil || b != nil {
		t.Errorf("无福利应返回 (nil, nil): b=%v err=%v", b, err)
	}
	// 4004 但文案不符 = 真错误。
	if _, err := parseBenefitBalance([]byte(`{"error_code":"4004","error_msg":"其他错误"}`)); err == nil {
		t.Error("码与文案都命中才算无福利")
	}
	ok, err := parseBenefitBalance([]byte(`{"error_code":"0000","result":{"channel":"codearts","daily_token_limit":1000,"daily_tokens_used":100}}`))
	if err != nil || ok == nil || ok.DailyTokenLimit != 1000 {
		t.Errorf("正常解析失败: %+v %v", ok, err)
	}
}

func TestParseCredAliases(t *testing.T) {
	raw := []byte(`{"access_key_id":"AK","secret_access_key":"SK","security_token":"ST",
		"expiration":"2026-09-27T16:17:00.327Z","account_id":"DOM","principal_id":"UID",
		"principal_urn":"iam::domain:user/GT-gitcode337577",
		"refresh_token":"a.b.c"}`)
	c, err := ParseCred(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.DomainID != "DOM" || c.UserID != "UID" {
		t.Errorf("身份别名解析失败: %+v", c)
	}
	// 导入侧 principal_urn 只做直接映射（URN→名字的切分在 JWT 身份链做）。
	if c.UserName != "iam::domain:user/GT-gitcode337577" {
		t.Errorf("principal_urn 应直接映射: %s", c.UserName)
	}
	ms, ok := c.ExpiresAtMs()
	if !ok || ms <= 0 {
		t.Errorf("expiration 别名解析失败: %s", c.ExpiresAt)
	}
}

func TestUUIDV4HexShape(t *testing.T) {
	id := uuidV4Hex()
	if len(id) != 32 {
		t.Fatalf("应 32 hex: %s", id)
	}
	if id[12] != '4' {
		t.Errorf("版本位应为 4: %s", id)
	}
	var any map[string]json.RawMessage
	_ = any
}
