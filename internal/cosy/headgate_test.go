package cosy

import (
	"errors"
	"strings"
	"testing"
)

// envelope 把内层 chunk JSON 包成嵌套 SSE 行。
func envelope(body string) string {
	return `data: {"headers":{},"body":` + strconvQuote(body) + `,"statusCodeValue":200}` + "\n\n"
}

func strconvQuote(s string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\n':
			sb.WriteString(`\n`)
		default:
			sb.WriteRune(r)
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// 首包门核心行为：错误帧在**任何内容之前** ⇒ HeadFault（调用方据此换号，
// 此时尚未写响应头——这正是修复「200 流错误」的关键）。
func TestPrefetchHeadGateFaultBeforeContent(t *testing.T) {
	up := envelope(`{"code":"403","message":"{\"code\":\"10605\",\"subCode\":\"RESOURCE_LIMIT\",\"isQueued\":true,\"serviceAvailable\":false,\"retryAfterSeconds\":30}"}`) +
		envelope(`{"choices":[{"delta":{"content":"hello"}}]}`) +
		"data: [DONE]\n\n"
	verdict, _, _, ie, err := PrefetchNestedSSE(strings.NewReader(up))
	if err != nil {
		t.Fatal(err)
	}
	if verdict != HeadFault || ie == nil {
		t.Fatalf("内容前的错误帧应判 HeadFault: verdict=%v ie=%v", verdict, ie)
	}
	if !ie.Busy() {
		t.Error("10605 应判为 Busy（容量类瞬时拒绝）")
	}
	if d := ie.RetryAfter(); d.Seconds() != 30 {
		t.Errorf("应提取 retryAfterSeconds=30: %v", d)
	}
}

// 有内容之后才出现的错误帧不属于首包门职责（HeadAnswered 已定）。
func TestPrefetchHeadGateAnsweredWins(t *testing.T) {
	up := envelope(`{"choices":[{"delta":{"content":"hi"}}]}`) +
		envelope(`{"code":"403","message":"boom"}`)
	verdict, pre, rest, ie, err := PrefetchNestedSSE(strings.NewReader(up))
	if err != nil || ie != nil {
		t.Fatalf("内容帧应判 Answered 且无 fault: verdict=%v ie=%v err=%v", verdict, ie, err)
	}
	if verdict != HeadAnswered {
		t.Fatalf("verdict 应为 HeadAnswered: %v", verdict)
	}
	if !strings.Contains(string(pre), `content`) || !strings.Contains(string(pre), `hi`) {
		t.Errorf("pre 必须含首个内容帧（不能丢字节）: %q", pre)
	}
	// rest 应能继续读出后续错误帧
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := rest.Read(buf)
		sb.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	if !strings.Contains(sb.String(), "boom") {
		t.Errorf("rest 应含后续帧: %q", sb.String())
	}
}

// 纯 role 帧（无正文）不算答案：继续等到真内容。
func TestPrefetchHeadGateRoleOnlyNotAnswer(t *testing.T) {
	up := envelope(`{"choices":[{"delta":{"role":"assistant"}}]}`) +
		envelope(`{"choices":[{"delta":{"content":"x"}}]}`)
	verdict, _, _, ie, err := PrefetchNestedSSE(strings.NewReader(up))
	if err != nil || ie != nil {
		t.Fatal(err)
	}
	if verdict != HeadAnswered {
		t.Fatalf("role 帧后应继续等到内容帧: %v", verdict)
	}
}

// 流干净结束但零内容 ⇒ HeadDone（空回答按失败处理，调用方换号）。
func TestPrefetchHeadGateEmptyDone(t *testing.T) {
	up := envelope(`{"choices":[{"delta":{"role":"assistant"}}]}`) + "data: [DONE]\n\n"
	verdict, _, _, ie, err := PrefetchNestedSSE(strings.NewReader(up))
	if err != nil || ie != nil {
		t.Fatal(err)
	}
	if verdict != HeadDone {
		t.Fatalf("零内容应判 HeadDone: %v", verdict)
	}
}

// 通告帧（[KIND]#payload）不是错误，不应中断首包门。
func TestPrefetchHeadGateControlFrameIgnored(t *testing.T) {
	up := envelope(`[NOTICE]#{"x":1}`) + envelope(`{"choices":[{"delta":{"content":"ok"}}]}`)
	verdict, _, _, ie, err := PrefetchNestedSSE(strings.NewReader(up))
	if err != nil || ie != nil {
		t.Fatal(err)
	}
	if verdict != HeadAnswered {
		t.Fatalf("通告帧后应继续到内容帧: %v", verdict)
	}
}

// StreamAsOpenAIPrefixed 必须把 pre 原样参与转换（不能重复也不能丢）。
func TestStreamAsOpenAIPrefixedNoLossNoDup(t *testing.T) {
	pre := []byte(envelope(`{"choices":[{"delta":{"content":"A"}}]}`))
	rest := strings.NewReader(envelope(`{"choices":[{"delta":{"content":"B"}}]}`) + "data: [DONE]\n\n")
	var out strings.Builder
	if err := StreamAsOpenAIPrefixed(&out, pre, rest, "m", nil); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if strings.Count(s, `"A"`) != 1 || strings.Count(s, `"B"`) != 1 {
		t.Errorf("pre/rest 内容应各出现一次: %s", s)
	}
	if !strings.Contains(s, "data: [DONE]") {
		t.Error("应补 [DONE]")
	}
}

// 上线后的错误分类钻进 error 帧类型（与旧内联实现一致，回归保护）。
func TestPrefetchFaultErrorType(t *testing.T) {
	up := envelope(`{"code":"4028","message":"destination-addr loopback not allowed"}`)
	_, _, _, ie, _ := PrefetchNestedSSE(strings.NewReader(up))
	if ie == nil {
		t.Fatal("应识别错误帧")
	}
	var target *InStreamError
	if !errors.As(error(ie), &target) {
		t.Error("InStreamError 应可 errors.As")
	}
	if ie.Busy() {
		t.Error("4028 不是 Busy（路由拒收，非容量）")
	}
}
