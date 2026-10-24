// chat.go CodeArts 对话：请求体预处理（关思考摘除/历史思考丢弃/tool_choice 折叠/
// max_tokens 钳制）→ 签名 → 首包门 → 流式透传 / 非流式聚合。
package codearts

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/hwauth"
)

// chatHeaders 对话请求头（全部参与签名）。
func chatHeaders(model, sessionID string, benefit bool) [][2]string {
	hs := [][2]string{
		{"Content-Type", "application/json"},
		{"Accept", "text/event-stream"},
		{"client_version", "Vscode_" + PluginVersion},
		{"Agent-Type", "ChatAgent"},
		{"X-Language", Language},
		{"is_confidential", "false"},
		{"plugin-name", PluginName},
		{"plugin-version", PluginVersion},
		// 上游把模型名也当请求头（三个键都要，少一个都不行）。
		{"model-id", model},
		{"model-name", model},
		{"x-model-id", model},
	}
	if sessionID != "" {
		hs = append(hs, [][2]string{{"User-Session-Id", sessionID}}...)
	}
	if benefit {
		hs = append(hs, [][2]string{{"maas_type", "benefit"}}...)
	}
	return hs
}

// prepareBody 出站预处理（按 agent2api chat.rs 顺序）：
//  1. model/stream 强制写
//  2. stream==true 注入 stream_options.include_usage
//  3. 摘掉「关思考」表达（reasoning_effort/reasoningEffort/effort ∈ {off,none}、
//     enable_thinking:false —— 本家没有关闭思考的档，带着整条拒收）
//  4. 丢历史思考（assistant 的 reasoning_content/reasoning 与 thinking/redacted_thinking
//     块；实测占体积 38-45%）
//  5. tool_choice 对象形态折成 "required"
//  6. max_tokens/max_completion_tokens > 65536 钳到 65536
func prepareBody(body map[string]any, model string, stream bool) {
	body["model"] = model
	body["stream"] = stream
	if stream {
		if so, ok := body["stream_options"].(map[string]any); ok {
			if _, has := so["include_usage"]; !has {
				so["include_usage"] = true
			}
		} else {
			body["stream_options"] = map[string]any{"include_usage": true}
		}
	}
	for _, key := range []string{"reasoning_effort", "reasoningEffort", "effort"} {
		if v, _ := body[key].(string); v == "off" || v == "none" {
			delete(body, key)
		}
	}
	if v, ok := body["enable_thinking"]; ok {
		if b, isBool := v.(bool); isBool && !b {
			delete(body, "enable_thinking")
		}
	}
	stripHistoricalReasoning(body)
	foldForcedToolChoice(body)
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if n, ok := body[key].(float64); ok && n > float64(MaxOutputTokens) {
			body[key] = float64(MaxOutputTokens)
		}
	}
}

// stripHistoricalReasoning 丢 assistant 历史思考。
func stripHistoricalReasoning(body map[string]any) {
	msgs, ok := body["messages"].([]any)
	if !ok {
		return
	}
	for _, mv := range msgs {
		m, ok := mv.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role != "assistant" {
			continue
		}
		delete(m, "reasoning_content")
		delete(m, "reasoning")
		if parts, ok := m["content"].([]any); ok {
			kept := make([]any, 0, len(parts))
			for _, pv := range parts {
				p, ok := pv.(map[string]any)
				if ok {
					if t, _ := p["type"].(string); t == "thinking" || t == "redacted_thinking" {
						continue
					}
				}
				kept = append(kept, pv)
			}
			m["content"] = kept
		}
	}
}

// foldForcedToolChoice 对象形态 tool_choice 折成 "required"（本家不收对象形态）。
func foldForcedToolChoice(body map[string]any) {
	tc, ok := body["tool_choice"].(map[string]any)
	if !ok {
		return
	}
	if t, _ := tc["type"].(string); t == "function" {
		body["tool_choice"] = "required"
	}
}

// ChatOutcome 一次对话的结果。
type ChatOutcome int

const (
	// ChatStreamed 流式已写出（成功或流内错误）。
	ChatStreamed ChatOutcome = iota
	// ChatCompleted 非流式完成。
	ChatCompleted
	// ChatRetryable 可换号重试（含流内故障/空回答）。
	ChatRetryable
)

// Fault 流内错误信封（agent2api stream_fault.rs 的判定）。
type Fault struct {
	Code    string
	Message string
	Status  int
}

// chatWithAccount 在一个账号上执行一次对话（含心跳会话）。
// onRetryable 命中可重试错误时回调（带分类状态码，供池冷却决策）。
func (g *Gateway) chatWithAccount(body []byte, model string, stream bool, benefit bool,
	cred *Credential, sessionID string) (io.ReadCloser, *Fault, error) {
	bmap := map[string]any{}
	if err := json.Unmarshal(body, &bmap); err != nil {
		return nil, nil, fmt.Errorf("请求体不是 JSON 对象: %w", err)
	}
	prepareBody(bmap, model, stream)
	payload, err := json.Marshal(bmap)
	if err != nil {
		return nil, nil, err
	}
	endpoint := BaseURL + ChatPath
	signed, err := hwauth.SignSDK(http.MethodPost, endpoint, chatHeaders(model, sessionID, benefit), payload, cred.Signing(), false)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, err
	}
	for _, kv := range signed.Headers {
		req.Header.Set(kv[0], kv[1])
	}
	resp, err := g.upstream.HTTP.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("CodeArts 对话传输失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		resp.Body.Close()
		msg := "上游返回 " + fmt.Sprint(resp.StatusCode)
		if d := extractErrMsg(raw); d != "" {
			msg += ": " + d
		}
		return nil, &Fault{Code: classifyFaultCode(resp.StatusCode, string(raw)), Message: msg, Status: resp.StatusCode}, nil
	}
	// 首包门：拿到第一个有内容的帧前按住响应（64 KiB 上限）。
	seen, rc, fault := prefetchHead(resp)
	if fault != nil {
		return nil, fault, nil
	}
	wrapped := &prefixReader{prefix: seen, rest: rc}
	return wrapped, nil, nil
}

// extractErrMsg 从错误体提 message/error_msg/error。
func extractErrMsg(raw []byte) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return truncate(strings.TrimSpace(string(raw)), 300)
	}
	for _, k := range []string{"message", "error_msg", "error"} {
		if s, _ := m[k].(string); s != "" {
			return s
		}
	}
	return truncate(strings.TrimSpace(string(raw)), 300)
}

// classifyFaultCode 403+insufficient quota → insufficient_quota；429 → rate_limit_exceeded。
func classifyFaultCode(status int, body string) string {
	lowered := strings.ToLower(body)
	if status == 403 || strings.Contains(lowered, "insufficient quota") {
		return "insufficient_quota"
	}
	if status == 429 || strings.Contains(lowered, "rate limit") {
		return "rate_limit_exceeded"
	}
	return ""
}

// prefixReader 已预读字节在前、剩余流在后。
type prefixReader struct {
	prefix []byte
	rest   io.ReadCloser
	eaten  bool
}

func (p *prefixReader) Read(b []byte) (int, error) {
	if !p.eaten {
		p.eaten = true
		if len(p.prefix) > 0 {
			n := copy(b, p.prefix)
			return n, nil
		}
	}
	return p.rest.Read(b)
}

func (p *prefixReader) Close() error { return p.rest.Close() }

// prefetchHead 把上游流拉到第一个有内容的帧（agent2api chat.rs prefetch_head +
// stream_fault 的 Go 移植）。
func prefetchHead(resp *http.Response) ([]byte, io.ReadCloser, *Fault) {
	const headLimit = 64 << 10
	var seen bytes.Buffer
	text := ""
	reader := bufio.NewReaderSize(resp.Body, headLimit)
	buf := make([]byte, 8<<10)
	for {
		for _, frame := range dataLines(text) {
			if payload, fault := frameFault(frame); fault != nil {
				resp.Body.Close()
				return nil, nil, fault
			} else if payload {
				_ = seen // 已有内容：整个 seen 原样补给客户端
				return seen.Bytes(), resp.Body, nil
			}
		}
		n, err := reader.Read(buf)
		if n > 0 {
			seen.Write(buf[:n])
			text += string(buf[:n])
			// 只保留尾部 256KB 文本做帧判定（seen 才是要下发的完整字节）。
			if len(text) > 256<<10 {
				text = text[len(text)-(128<<10):]
			}
			if seen.Len() > headLimit {
				resp.Body.Close()
				return nil, nil, &Fault{Code: "", Message: fmt.Sprintf(
					"CodeArts 上游在给出内容前已发送超过 %d KiB 的元数据（按失败处理）", headLimit/1024), Status: 502}
			}
		}
		if err != nil {
			resp.Body.Close()
			return nil, nil, &Fault{Code: "", Message: "CodeArts 上游流结束但未返回任何内容（空回答按失败处理）", Status: 502}
		}
	}
}

// dataLines 从文本里取完整成对 \n\n 结束的 data: 帧载荷。
func dataLines(text string) []string {
	var out []string
	for _, block := range strings.Split(text, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if payload, ok := strings.CutPrefix(line, "data:"); ok {
				out = append(out, payload)
			}
		}
	}
	return out
}

// frameFault 判一帧是不是故障帧。返回 (hasAnswer, fault)：有答案就不算故障
//（丢内容比漏报一次故障更糟）。
func frameFault(payload string) (bool, *Fault) {
	payload = strings.TrimSpace(payload)
	if payload == "" || payload == "[DONE]" {
		return false, nil
	}
	var frame map[string]any
	if json.Unmarshal([]byte(payload), &frame) != nil {
		return true, nil // 非 JSON：当内容透传
	}
	code, _ := frame["error_code"].(string)
	code = strings.TrimSpace(code)
	if code == "" || code == "0" || code == "0000" || strings.EqualFold(code, "success") {
		return true, nil // 成功码/普通数据帧
	}
	if frameHasAnswer(frame) {
		return true, nil
	}
	message := "上游报告 " + code
	if detail, _ := frame["error_msg"].(string); strings.TrimSpace(detail) != "" {
		message += "：" + strings.TrimSpace(detail)
	}
	if details, ok := frame["details"].([]any); ok {
		for _, dv := range details {
			dm, _ := dv.(map[string]any)
			if dm == nil {
				continue
			}
			detail := ""
			for _, k := range []string{"error_msg", "error_code"} {
				if s, _ := dm[k].(string); s != "" {
					detail = s
					break
				}
			}
			if detail == "" || strings.Contains(message, detail) || strings.Contains(detail, ":") {
				continue
			}
			message += "（" + detail + "）"
			break
		}
	}
	lowered := strings.ToLower(message)
	status := 502
	switch {
	case strings.Contains(lowered, "insufficient quota"):
		status = 403
	case strings.Contains(code, "429") || strings.Contains(lowered, "rate limit") || strings.Contains(lowered, "too many"):
		status = 429
	}
	return false, &Fault{Code: code, Message: message, Status: status}
}

// frameHasAnswer 帧里带了任何答案就不判故障（delta/message/text/tool_calls）。
func frameHasAnswer(frame map[string]any) bool {
	if deltaHasContent(frame["delta"]) {
		return true
	}
	if choices, ok := frame["choices"].([]any); ok {
		for _, cv := range choices {
			cm, _ := cv.(map[string]any)
			if cm == nil {
				continue
			}
			if deltaHasContent(cm["delta"]) || deltaHasContent(cm["message"]) {
				return true
			}
			if s, _ := cm["text"].(string); s != "" && s != "[DONE]" {
				return true
			}
		}
	}
	if s, _ := frame["text"].(string); s != "" && s != "[DONE]" {
		return true
	}
	return false
}

func deltaHasContent(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	switch c := m["content"].(type) {
	case string:
		if c != "" {
			return true
		}
	case []any:
		for _, pv := range c {
			pm, _ := pv.(map[string]any)
			if s, _ := pm["text"].(string); s != "" {
				return true
			}
		}
	}
	if s, _ := m["reasoning_content"].(string); s != "" {
		return true
	}
	if tc, ok := m["tool_calls"].([]any); ok && len(tc) > 0 {
		return true
	}
	return false
}

// Aggregate 非流式聚合（SSE → 一条 completion）。返回 (响应体, fault)。
func Aggregate(rc io.ReadCloser, model string) ([]byte, *Fault) {
	defer rc.Close()
	type toolCall struct {
		ID   string `json:"id,omitempty"`
		Type string `json:"type,omitempty"`
		Function struct {
			Name      string `json:"name,omitempty"`
			Arguments string `json:"arguments"`
		} `json:"function"`
		Index int `json:"index"`
	}
	var (
		content, reasoning strings.Builder
		tools              = map[int]*toolCall{}
		toolOrder          []int
		usage              map[string]any
		finish             string
		chatID             string
		seenDone           bool
	)
	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		line := scanner.Text()
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "[DONE]" {
			seenDone = true
			continue
		}
		if has, fault := frameFault(payload); fault != nil {
			return nil, fault
		} else if has {
			// 内容帧也要聚合。
		}
		var frame struct {
			ID      string `json:"id"`
			Usage   map[string]any `json:"usage"`
			Choices []struct {
				Delta struct {
					Content      *string     `json:"content"`
					Reasoning    *string     `json:"reasoning_content"`
					ToolCalls    []toolCall  `json:"tool_calls"`
				} `json:"delta"`
				FinishReason any `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &frame) != nil {
			continue
		}
		if frame.ID != "" && chatID == "" {
			chatID = frame.ID
		}
		if frame.Usage != nil {
			usage = frame.Usage
		}
		for _, ch := range frame.Choices {
			if ch.Delta.Content != nil {
				content.WriteString(*ch.Delta.Content)
			}
			if ch.Delta.Reasoning != nil {
				reasoning.WriteString(*ch.Delta.Reasoning)
			}
			for _, tc := range ch.Delta.ToolCalls {
				slot := tools[tc.Index]
				if slot == nil {
					slot = &toolCall{Index: tc.Index, Type: tc.Type, ID: tc.ID}
					slot.Function.Name = tc.Function.Name
					tools[tc.Index] = slot
					toolOrder = append(toolOrder, tc.Index)
				}
				if slot.ID == "" && tc.ID != "" {
					slot.ID = tc.ID
				}
				if slot.Function.Name == "" && tc.Function.Name != "" {
					slot.Function.Name = tc.Function.Name
				}
				slot.Function.Arguments += tc.Function.Arguments
			}
			if s, ok := ch.FinishReason.(string); ok && s != "" {
				finish = s
			}
		}
	}
	hasContent := content.Len() > 0 || reasoning.Len() > 0 || len(toolOrder) > 0
	if !hasContent {
		msg := "CodeArts 上游流结束但未返回任何内容"
		if !seenDone {
			msg = "CodeArts 上游流中断（未完成）"
		}
		return nil, &Fault{Code: "", Message: msg, Status: 502}
	}
	if finish == "" {
		if len(toolOrder) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	if chatID == "" {
		chatID = "chatcmpl-codearts-" + uuidV4Hex()[:12]
	}
	msg := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	var toolList []any
	for _, idx := range toolOrder {
		if tc := tools[idx]; tc != nil {
			toolList = append(toolList, tc)
		}
	}
	if len(toolList) > 0 {
		msg["tool_calls"] = toolList
	}
	choice := map[string]any{"index": 0, "message": msg, "finish_reason": finish}
	out := map[string]any{
		"id": chatID, "object": "chat.completion",
		"created": time.Now().Unix(), "model": model,
		"choices": []any{choice},
	}
	if usage != nil {
		out["usage"] = usage
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, &Fault{Code: "", Message: "聚合响应序列化失败: " + err.Error(), Status: 502}
	}
	return raw, nil
}
