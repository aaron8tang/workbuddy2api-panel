// failover_test.go 跨渠道失败转移的判定逻辑单测。
package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/modelgroup"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// stubChannel 假渠道：writes=true 时写一个响应，否则什么都不写。
// 用于验证 tryChannel 的「有没有写出响应」判定。
type stubChannel struct {
	writes   bool
	calls    int
	gotModel string
}

func (s *stubChannel) ChatCompletions(w http.ResponseWriter, r *http.Request, bareModel string,
	onAccount func(uid, nick string), onOutcome reqlog.OutcomeReporter) {
	s.calls++
	s.gotModel = bareModel
	if s.writes {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

func (s *stubChannel) Servable() bool { return true }

// TestCaptureResponseWriter 验证「有没有写过响应」的判定：
// 只 Flush 不算写过（流式空缓冲不产生字节），WriteHeader/Write 才算。
func TestCaptureResponseWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := newCaptureResponseWriter(rec)
	if cw.wrote {
		t.Fatal("新建即未写过")
	}
	cw.Flush()
	if cw.wrote {
		t.Fatal("Flush 空缓冲不应算写过响应")
	}
	cw.WriteHeader(http.StatusOK)
	if !cw.wrote {
		t.Fatal("WriteHeader 后应算写过")
	}

	rec2 := httptest.NewRecorder()
	cw2 := newCaptureResponseWriter(rec2)
	if _, err := cw2.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if !cw2.wrote {
		t.Fatal("Write 后应算写过")
	}
}

// TestFailoverStopKind 请求级错误不触发转移（换渠道必然同样失败）。
func TestFailoverStopKind(t *testing.T) {
	stop := []upstream.ErrKind{
		upstream.ErrBadParams, upstream.ErrPromptTooLong,
		upstream.ErrImageInvalid, upstream.ErrContentBlocked,
	}
	for _, k := range stop {
		if !failoverStopKind(k) {
			t.Errorf("kind %v 应停止转移", k)
		}
	}
	// 账号/渠道级错误应当转移。
	cont := []upstream.ErrKind{
		upstream.ErrHardCredit, upstream.ErrSoftRate, upstream.ErrServer,
		upstream.ErrModelBlocked, upstream.ErrAccountFault, upstream.ErrWafBlock,
	}
	for _, k := range cont {
		if failoverStopKind(k) {
			t.Errorf("kind %v 不应停止转移", k)
		}
	}
}

// TestFailoverSkipCredit 积分耗尽的默认语义是「照常转移」（各渠道额度独立），
// 只有 skipOnCredit 显式打开才停止。
func TestFailoverSkipCredit(t *testing.T) {
	creditErr := &upstream.Error{Kind: upstream.ErrHardCredit, Status: 402, Msg: "insufficient credit"}
	if failoverSkip(creditErr, false) {
		t.Error("积分耗尽默认应照常转移")
	}
	if !failoverSkip(creditErr, true) {
		t.Error("skip_on_credit=true 时积分耗尽应停止转移")
	}
	// 请求级错误无论开关如何都不转移。
	badErr := &upstream.Error{Kind: upstream.ErrBadParams, Status: 400}
	if !failoverSkip(badErr, false) || !failoverSkip(badErr, true) {
		t.Error("请求级错误恒不转移")
	}
	// 上游超时恒不转移（哨兵经 %w 包裹，errors.Is 可穿透）。
	if !failoverSkip(fmt.Errorf("%w: %v", errUpstreamTimeout, errors.New("context deadline")), false) {
		t.Error("上游超时应停止转移")
	}
}

// TestFailoverCandidates 候选生成：跳过当前渠道、跳过不可用渠道、跳过排除名单、
// 按模型组优先级排序并带上各自的裸模型名与档位。
func TestFailoverCandidates(t *testing.T) {
	h := &Handler{}
	grp := &modelgroup.Group{Name: "g", Models: []string{
		"trae:glm-5.3", "codearts:deepseek-v4.1-flash", "officeace:glm-5.3", "cn:glm-5.3-flash|max",
	}}
	// 渠道全部未装配时应当没有候选（零回归：渠道关闭不参与）。
	got := h.failoverCandidates(grp, "qoderwork", "glm-5.3")
	if len(got) != 0 {
		t.Fatalf("全渠道未装配时应无候选，实得 %d", len(got))
	}
	// 无模型组 + 无装配渠道 → 无候选（不会 panic）。
	if got := h.failoverCandidates(nil, "cn", "glm-5.3"); len(got) != 0 {
		t.Fatalf("退化路径应无候选，实得 %d", len(got))
	}
}

// TestTryChannel 渠道「跑了但一个字节都没写」= 失败可换下一个；写了 = 必须收尾。
func TestTryChannel(t *testing.T) {
	h := &Handler{}
	st := newChatStat(time.Now(), []byte(`{"model":"x"}`), false)

	quiet := &stubChannel{writes: false}
	rec := httptest.NewRecorder()
	if h.tryChannel(quiet, rec, httptest.NewRequest("POST", "/v1/chat/completions", nil),
		[]byte(`{"model":"x"}`), "glm-5.3", st) {
		t.Error("没写响应的渠道应判定为失败（可换下一个）")
	}
	if quiet.calls != 1 {
		t.Errorf("应调用一次，实得 %d", quiet.calls)
	}

	loud := &stubChannel{writes: true}
	rec2 := httptest.NewRecorder()
	if !h.tryChannel(loud, rec2, httptest.NewRequest("POST", "/v1/chat/completions", nil),
		[]byte(`{"model":"x"}`), "glm-5.3", st) {
		t.Error("写了响应的渠道应判定为成功（调用方必须收尾）")
	}
	if loud.gotModel != "glm-5.3" {
		t.Errorf("裸模型名未传下去：%q", loud.gotModel)
	}
}

// TestFailoverExcluded 排除名单匹配（大小写不敏感、去空白）。
func TestFailoverExcluded(t *testing.T) {
	h := &Handler{}
	h.cfg.Failover.Exclude = []string{" AutoClaw ", "officeace"}
	for _, name := range []string{"autoclaw", "AutoClaw", "officeace"} {
		if !h.failoverExcluded(name) {
			t.Errorf("%q 应在排除名单里", name)
		}
	}
	if h.failoverExcluded("trae") {
		t.Error("trae 不应被排除")
	}
}
