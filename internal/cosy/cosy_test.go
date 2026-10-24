package cosy

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestEncodeBodyRoundTrip(t *testing.T) {
	raw := []byte(`{"hello":"world","n":42,"中文":"编码"}`)
	enc := EncodeBody(raw)
	if string(enc) == string(raw) {
		t.Fatal("encoding must transform body")
	}
	dec, err := DecodeBody(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(dec) != string(raw) {
		t.Errorf("roundtrip mismatch: %q != %q", dec, raw)
	}
}

func TestPayloadBase64Orders(t *testing.T) {
	id := Identity{Name: "n", Aid: "a", UID: "u", SecurityOAuthToken: "dt", RefreshToken: "drt"}
	fp := NewFingerprint()
	sess, err := NewSession(fp, id)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	for _, order := range []PayloadOrder{OrderSorted, OrderStruct} {
		p := Payload{Order: order, Version: "v1", CosyVersion: "1.0.0", IDEVersion: "", RequestID: "rid"}
		b64, err := sess.PayloadBase64(p)
		if err != nil {
			t.Fatalf("order %d: %v", order, err)
		}
		if _, err := base64.StdEncoding.DecodeString(b64); err != nil {
			t.Errorf("payload must be standard base64, got %q", b64)
		}
	}
}

func TestAuthorizationShape(t *testing.T) {
	id := Identity{Name: "n", Aid: "a", UID: "u", SecurityOAuthToken: "dt", RefreshToken: "drt"}
	sess, err := NewSession(NewFingerprint(), id)
	if err != nil {
		t.Fatal(err)
	}
	p := Payload{Order: OrderStruct, Version: "v1", CosyVersion: "1.0.0", RequestID: "rid"}
	auth, err := sess.Authorization([]byte(`{}`), "/algo/api/v2/model/list", p, time.Unix(1792000000, 0))
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	// 形态：Bearer COSY.<payload>.<md5>
	const prefix = "Bearer COSY."
	if !strings.HasPrefix(auth, prefix) {
		t.Fatalf("auth prefix: %q", auth)
	}
	rest := strings.TrimPrefix(auth, prefix)
	sigIdx := strings.LastIndexByte(rest, '.')
	if sigIdx < 0 {
		t.Fatalf("missing signature segment: %q", auth)
	}
	var payload map[string]any
	raw, err := base64.StdEncoding.DecodeString(rest[:sigIdx])
	if err != nil {
		t.Fatalf("payload b64: %v", err)
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("payload json: %v", err)
	}
	// OrderStruct 字段序：version, requestId, info, cosyVersion, ideVersion
	for _, key := range []string{"version", "requestId", "info", "cosyVersion", "ideVersion"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("payload missing %q: %v", key, payload)
		}
	}
}

func TestSignedPath(t *testing.T) {
	cases := map[string]string{
		"https://gateway.x.com/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1": "/api/v2/service/pro/sse/agent_chat_generation",
		"https://gw.x.com/algo/api/v2/model/list?Encode=1": "/api/v2/model/list",
		"https://gw.x.com/plain/path": "/plain/path",
	}
	for in, want := range cases {
		if got := SignedPath(in); got != want {
			t.Errorf("SignedPath(%q)=%q want %q", in, got, want)
		}
	}
}

func TestDeriveRuntimeReusableSession(t *testing.T) {
	id := Identity{Name: "n", Aid: "u1", UID: "u1", SecurityOAuthToken: "dt-1", RefreshToken: "drt-1"}
	fp := NewFingerprint()
	key, info, err := DeriveRuntime(id)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	sess, err := NewSessionFromRuntime(fp, id, key, info)
	if err != nil {
		t.Fatalf("from runtime: %v", err)
	}
	// 复用会话的 CosyKey/Info 必须与派生值一致
	if sess.CosyKey != key || sess.Info != info {
		t.Error("reused session must carry derived runtime fields")
	}
	// token 轮换后 identity 变化 → 必须重派生
	id2 := id
	id2.SecurityOAuthToken = "dt-2"
	key2, _, err := DeriveRuntime(id2)
	if err != nil {
		t.Fatal(err)
	}
	if key2 == key {
		t.Error("rotated identity must derive new runtime key")
	}
	// 不完整输入必须报错
	if _, err := NewSessionFromRuntime(fp, id, "", ""); err == nil {
		t.Error("incomplete runtime must error")
	}
	if _, err := NewSessionFromRuntime(Fingerprint{}, id, key, info); err == nil {
		t.Error("incomplete fingerprint must error")
	}
}

func TestClassifyPriority(t *testing.T) {
	// hard marker 优先于状态码：402 余额文案 → Hard
	if got := Classify(402, `{"message":"insufficient balance"}`); got != ErrHardCredit {
		t.Errorf("hard marker: %v", got)
	}
	// 429 → Soft
	if got := Classify(429, `rate limited`); got != ErrSoftRate {
		t.Errorf("429: %v", got)
	}
	// 401 TOKEN_EXPIRE → TokenExpired（refresh 可修复；drt 死亡才是 SessionDead）
	if got := Classify(401, `TOKEN_EXPIRED`); got != ErrTokenExpired {
		t.Errorf("401: %v", got)
	}
}

// nestedSSE 构造嵌套 SSE 响应体：外层 envelope 包内层 JSON 字符串。
func nestedSSE(bodies ...string) string {
	var b strings.Builder
	for _, body := range bodies {
		env := map[string]any{"body": body, "statusCodeValue": 200, "statusCode": "OK"}
		raw, _ := json.Marshal(env)
		b.WriteString("data: " + string(raw) + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func TestStreamAsOpenAIErrorFrame(t *testing.T) {
	up := nestedSSE(
		`{"code":"403","message":"{\"code\":\"10605\",\"msg\":\"RESOURCE_LIMIT\"}"}`,
		`{"id":"x","choices":[{"delta":{"content":"should not leak"}}]}`,
	)
	var out strings.Builder
	err := StreamAsOpenAI(&out, strings.NewReader(up), "TestModel", nil)
	var ie *InStreamError
	if !errors.As(err, &ie) {
		t.Fatalf("want InStreamError, got %v", err)
	}
	if ie.Code != "403" {
		t.Errorf("code = %q", ie.Code)
	}
	if !ie.Busy() {
		t.Error("10605 RESOURCE_LIMIT must classify as busy")
	}
	s := out.String()
	if strings.Contains(s, "should not leak") {
		t.Error("chunks after error frame must not be forwarded")
	}
	if !strings.Contains(s, `"type":"upstream_error"`) || !strings.Contains(s, "data: [DONE]") {
		t.Errorf("missing proper error event/DONE: %s", s)
	}
}

func TestStreamAsOpenAINormalChunksUntouched(t *testing.T) {
	up := nestedSSE(
		`{"id":"1","choices":[{"delta":{"role":"assistant","content":"hi"}}]}`,
		`{"id":"1","choices":[],"usage":{"total_tokens":9}}`,
	)
	var out strings.Builder
	if err := StreamAsOpenAI(&out, strings.NewReader(up), "TestModel", nil); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if !strings.Contains(out.String(), `"content":"hi"`) || !strings.Contains(out.String(), `"total_tokens":9`) {
		t.Errorf("normal chunks/usage must pass through: %s", out.String())
	}
}

func TestAggregateNestedErrorFrame(t *testing.T) {
	// 无内容 + 错误帧 → 返回 InStreamError
	up := nestedSSE(`{"code":"400","message":"header destination-addr invalid: 127.0.0.1:7863, specifing loopback address is NOT allowed (HTTP Status: 400) (4028)"}`)
	_, err := AggregateNested(strings.NewReader(up), "M")
	var ie *InStreamError
	if !errors.As(err, &ie) || !strings.Contains(ie.Msg, "destination-addr") {
		t.Fatalf("want InStreamError with msg, got %v", err)
	}
	if ie.Busy() {
		t.Error("4028 loopback rejection must not classify as busy")
	}
	// 已有内容 + 错误帧 → 降级为部分响应
	up2 := nestedSSE(
		`{"id":"2","choices":[{"delta":{"content":"partial"}}]}`,
		`{"code":"403","message":"trailing notice"}`,
	)
	resp, err := AggregateNested(strings.NewReader(up2), "M")
	if err != nil {
		t.Fatalf("partial content must not fail: %v", err)
	}
	chs, _ := resp["choices"].([]any)
	c0, _ := chs[0].(map[string]any)
	cmsg, _ := c0["message"].(map[string]any)
	if cmsg["content"] != "partial" {
		t.Errorf("partial content lost: %v", resp)
	}
}

func TestErrFrameOfIgnoresNormalShapes(t *testing.T) {
	if _, ok := errFrameOf(map[string]any{"choices": []any{}, "code": "x", "message": "y"}); ok {
		t.Error("chunk with choices key must never be an error frame")
	}
	if _, ok := errFrameOf(map[string]any{"code": "x"}); ok {
		t.Error("missing message must not be an error frame")
	}
	if _, ok := errFrameOf(map[string]any{"message": "y"}); ok {
		t.Error("missing code must not be an error frame")
	}
}

// TestBusyRealFrameShapes 实测 10605 帧形态（外层 code=403 + message 内嵌 JSON）：
// Busy 必须识别；provider_error 同为容量/上游临时故障类；RetryAfter 从内嵌 JSON
// 提取 retryAfterSeconds。
func TestBusyRealFrameShapes(t *testing.T) {
	// 实测帧：qoder_chat 日志原样
	ie := &InStreamError{
		Code: "403",
		Msg:  `{"isQueued":true,"modelKey":"qfmodel","queueCount":0,"queueType":"p4","retryAfterSeconds":30,"serviceAvailable":false,"subCode":"RESOURCE_LIMIT","waitTime":30}`,
	}
	if !ie.Busy() {
		t.Error("real 10605 inner-JSON frame must be Busy")
	}
	if d := ie.RetryAfter(); d != 30*time.Second {
		t.Errorf("RetryAfter = %v, want 30s", d)
	}
	// 外层 code 直接是 10605 的形态
	if !(&InStreamError{Code: "10605", Msg: "boom"}).Busy() {
		t.Error("code=10605 must be Busy")
	}
	// provider_error：上游临时故障，同为 Busy
	if !(&InStreamError{Code: "provider_error", Msg: "Error in upstream response"}).Busy() {
		t.Error("provider_error must be Busy")
	}
	// 4028 loopback 拒收不是 Busy（消息非 JSON、无资源限制特征）
	if (&InStreamError{Code: "400", Msg: "header destination-addr invalid: ... (4028)"}).Busy() {
		t.Error("4028 must not be Busy")
	}
	// 无 retryAfterSeconds 时 RetryAfter 返回 0
	if d := (&InStreamError{Code: "provider_error", Msg: "Error in upstream response"}).RetryAfter(); d != 0 {
		t.Errorf("RetryAfter no field = %v, want 0", d)
	}
}

// TestPartialResponseCarriesError 非流式部分响应降级：错误帧在有内容之后到达时
// 内容保留、顶层 error 字段携带上游错误（客户端两侧信息都拿得到）。
func TestPartialResponseCarriesError(t *testing.T) {
	up := nestedSSE(
		`{"id":"3","choices":[{"delta":{"content":"partial answer"}}]}`,
		`{"code":"provider_error","message":"Error in upstream response"}`,
	)
	resp, err := AggregateNested(strings.NewReader(up), "M")
	if err != nil {
		t.Fatalf("partial content must not fail: %v", err)
	}
	chs, _ := resp["choices"].([]any)
	c0, _ := chs[0].(map[string]any)
	cmsg, _ := c0["message"].(map[string]any)
	if cmsg["content"] != "partial answer" {
		t.Errorf("content lost: %v", resp)
	}
	eobj, ok := resp["error"].(map[string]any)
	if !ok || eobj["code"] != "provider_error" {
		t.Errorf("error field missing/wrong: %v", resp["error"])
	}
}
