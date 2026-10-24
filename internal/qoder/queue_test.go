// queue_test.go 模型排队协议单测：peek/rejoin 流头回灌、10605 判定、
// queue/status 响应剥壳解析。
package qoder

import (
	"io"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cosy"
)

// nested 帧构造（对齐 cosy 包内格式：data: <envelope>\n\n）。
func qFrame(body string) string {
	env := `{"body":` + quoteJSON(body) + `,"statusCodeValue":200,"statusCode":"OK"}`
	return "data: " + env + "\n\n"
}

func quoteJSON(s string) string {
	b := []byte{'"'}
	for _, c := range s {
		switch c {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\n':
			b = append(b, '\\', 'n')
		default:
			b = append(b, byte(c))
		}
	}
	return string(append(b, '"'))
}

const queuedBody = `{"code":"403","message":"{\"code\":\"10605\",\"isQueued\":true,\"modelKey\":\"qfmodel\",\"queueCount\":0,\"queueType\":\"p4\",\"retryAfterSeconds\":30,\"serviceAvailable\":false,\"subCode\":\"RESOURCE_LIMIT\",\"waitTime\":30}","subCode":"RESOURCE_LIMIT"}`

func TestPeekQueueFrameQueued(t *testing.T) {
	rc := io.NopCloser(strings.NewReader(qFrame(queuedBody) + qFrame(`{"id":"x","choices":[]}`)))
	peek, err := peekQueueFrame(rc)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if peek.queued == nil {
		t.Fatal("expected 10605 queued frame")
	}
	if !isModelQueued(peek.queued) {
		t.Errorf("isModelQueued = false for %v", peek.queued)
	}
	q := parseQueueInfoFromError(peek.queued)
	if q.QueueType != "p4" {
		t.Errorf("queueType = %q, want p4", q.QueueType)
	}
	if q.wait() != 30 {
		t.Errorf("wait = %d, want 30", q.wait())
	}
	if q.drained() {
		t.Error("isQueued=true frame must not be drained")
	}
}

func TestPeekQueueFrameNormal(t *testing.T) {
	normal := qFrame(`{"id":"1","choices":[{"delta":{"content":"hi"}}]}`)
	rc := io.NopCloser(strings.NewReader(normal + qFrame(`{"id":"1","choices":[{"delta":{"content":" there"}}]}`)))
	peek, err := peekQueueFrame(rc)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if peek.queued != nil {
		t.Fatalf("normal first frame misread as queued: %v", peek.queued)
	}
	// rejoin 后可完整读出全部字节
	full, err := io.ReadAll(peek.rejoin(rc))
	if err != nil {
		t.Fatalf("read rejoined: %v", err)
	}
	if string(full) != normal+qFrame(`{"id":"1","choices":[{"delta":{"content":" there"}}]}`) {
		t.Errorf("rejoin lost/duplicated bytes: %q", string(full))
	}
}

func TestIsModelQueuedVariants(t *testing.T) {
	cases := []struct {
		name string
		ie   *cosy.InStreamError
		want bool
	}{
		{"outer 10605", &cosy.InStreamError{Code: "10605", Msg: "queued"}, true},
		{"inner json", &cosy.InStreamError{Code: "403", Msg: `{"code":"10605","subCode":"RESOURCE_LIMIT"}`}, true},
		{"resource limit subcode", &cosy.InStreamError{Code: "403", Msg: `{"subCode":"RESOURCE_LIMIT"}`}, true},
		{"provider_error", &cosy.InStreamError{Code: "provider_error", Msg: "Error in upstream response"}, false},
		{"4028", &cosy.InStreamError{Code: "400", Msg: "header destination-addr invalid (4028)"}, false},
	}
	for _, tc := range cases {
		if got := isModelQueued(tc.ie); got != tc.want {
			t.Errorf("%s: isModelQueued = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestParseQueueInfoPeeling(t *testing.T) {
	// 顶层直给
	q := parseQueueInfo([]byte(`{"isQueued":true,"serviceAvailable":false,"queueCount":3,"queueType":"p4","retryAfterSeconds":15,"waitTime":15}`))
	if q.QueueCount == nil || *q.QueueCount != 3 || q.QueueType != "p4" || q.wait() != 15 {
		t.Fatalf("direct parse: %+v", q)
	}
	// data 包层
	q = parseQueueInfo([]byte(`{"data":{"isQueued":false,"serviceAvailable":true}}`))
	if !q.drained() {
		t.Fatalf("data-wrapped: %+v", q)
	}
	// body 内嵌 JSON 字符串
	q = parseQueueInfo([]byte(`{"body":"{\"isQueued\":true,\"waitTime\":9}"}`))
	if q.wait() != 9 {
		t.Fatalf("body-wrapped: %+v", q)
	}
	// 空响应
	if q := parseQueueInfo([]byte(`{}`)); q.drained() {
		t.Fatal("empty object must not be drained")
	}
}
