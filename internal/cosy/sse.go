package cosy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// SSE 帧结构与两个站点一致：外层 envelope 的 body 字段是内层 OpenAI chunk 的
// JSON 字符串（双重包装），需二次解析。结束标记可能是 body=="[DONE]"，也可能
// 是 "event:finish"（Qoder 桌面端口径）。

// Envelope SSE 外层帧。
type Envelope struct {
	Headers         map[string][]string `json:"headers"`
	Body            string              `json:"body"`
	StatusCodeValue int                 `json:"statusCodeValue"`
	StatusCode      string              `json:"statusCode"`
}

// ParseNestedSSE 逐行解析嵌套 SSE，每个有效 chunk 调 onChunk。
//   - body == "[DONE]" 或 "event:finish" → 正常结束
//   - body 二次 unmarshal 失败 → 跳过该行（不致命）
//   - "[KIND]#" 前缀的通告帧 → 交给 onControl（可为 nil，忽略）
func ParseNestedSSE(r io.Reader, onChunk func(map[string]any) error, onControl func(kind, payload string)) error {
	br := bufio.NewReaderSize(r, 256*1024)
	var event strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "event:"):
			if event.Len() > 0 {
				event.WriteByte(' ')
			}
			event.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "event:")))
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if stop, serr := handleDataLine(payload, event.String(), onChunk, onControl); stop || serr != nil {
				return serr
			}
			event.Reset()
		}
		if err == io.EOF {
			return nil
		}
	}
}

// handleDataLine 处理一行 data。返回 stop=true 表示流已结束。
func handleDataLine(payload, eventName string, onChunk func(map[string]any) error, onControl func(kind, payload string)) (bool, error) {
	if strings.EqualFold(strings.TrimSpace(eventName), "finish") {
		return true, nil
	}
	if payload == "" || payload == "[DONE]" {
		return false, nil
	}
	var env Envelope
	if json.Unmarshal([]byte(payload), &env) != nil {
		return false, nil
	}
	body := strings.TrimSpace(env.Body)
	if body == "" || body == "[DONE]" {
		return false, nil
	}
	if kind, notice, ok := SplitControlFrame(body); ok {
		if onControl != nil {
			onControl(kind, notice)
		}
		return false, nil
	}
	var chunk map[string]any
	if json.Unmarshal([]byte(body), &chunk) != nil {
		return false, nil
	}
	if onChunk != nil {
		return false, onChunk(chunk)
	}
	return false, nil
}

// SplitControlFrame 判断 envelope body 是否为 "[KIND]#payload" 形式的通告帧。
func SplitControlFrame(body string) (kind, payload string, ok bool) {
	trimmed := strings.TrimSpace(body)
	if !strings.HasPrefix(trimmed, "[") {
		return "", "", false
	}
	end := strings.Index(trimmed, "]#")
	if end < 0 {
		return "", "", false
	}
	return trimmed[1:end], strings.TrimSpace(trimmed[end+2:]), true
}

// InStreamError 上游以 HTTP 200 + SSE 错误帧内嵌的业务错误。上游网关的容量/路由
// 类拒绝（如 10605 RESOURCE_LIMIT 队列满、4028 destination-addr 拒收）以
// {"code":...,"message":...}（无 choices）的帧出现在正常流里；原实现原样透传给
// 客户端成垃圾 chunk、面板记 200 成功、账号状态机毫无感知。显式识别后：客户端
// 收到标准 OpenAI error 事件，网关按类别处置账号、日志可诊断。
type InStreamError struct {
	Code string // 帧的 code 字段（"403"/"4028"/10605 等）
	Msg  string // message 原文（可能内嵌另一层 JSON）
}

func (e *InStreamError) Error() string {
	return fmt.Sprintf("upstream in-stream error code=%s: %s", e.Code, e.Msg)
}

// Busy 容量类瞬时错误判定：10605 RESOURCE_LIMIT / serviceAvailable=false /
// isQueued=true。真实帧形态（实测）：外层 code=403，message 是**内嵌 JSON 字符串**
// {"code":"10605","isQueued":true,"serviceAvailable":false,"subCode":"RESOURCE_LIMIT",
// "retryAfterSeconds":30,...}——因此除 Msg 全文子串匹配外，还要剥壳解析内嵌 JSON
// 读结构化字段，避免 message 转义形态变化导致漏判。Busy 不代表账号故障：先烧
// retryAfterSeconds（有值时）的短冷却让该号退出本轮轮转，请求换号重试。
func (e *InStreamError) Busy() bool {
	if e.Code == "10605" || e.Code == "provider_error" {
		return true
	}
	if strings.Contains(e.Msg, "RESOURCE_LIMIT") ||
		strings.Contains(e.Msg, `"serviceAvailable":false`) ||
		strings.Contains(e.Msg, `"isQueued":true`) {
		return true
	}
	// 内嵌 JSON 剥壳：message 是转义 JSON 字符串时解析其 subCode。
	var inner struct {
		SubCode string `json:"subCode"`
		Code    any    `json:"code"`
	}
	if err := json.Unmarshal([]byte(e.Msg), &inner); err == nil && inner.SubCode == "RESOURCE_LIMIT" {
		return true
	}
	return false
}

// RetryAfter 提取帧内的重试等待秒数（10605 的 retryAfterSeconds / waitTime）。
// 无该字段返回 0（调用方用默认冷却）。
func (e *InStreamError) RetryAfter() time.Duration {
	var inner struct {
		RetryAfterSeconds int `json:"retryAfterSeconds"`
		WaitTime          int `json:"waitTime"`
	}
	if err := json.Unmarshal([]byte(e.Msg), &inner); err != nil {
		return 0
	}
	if inner.RetryAfterSeconds > 0 {
		return time.Duration(inner.RetryAfterSeconds) * time.Second
	}
	if inner.WaitTime > 0 {
		return time.Duration(inner.WaitTime) * time.Second
	}
	return 0
}

// errFramePayload 把 InStreamError 组装成 OpenAI error 事件的 data 负载。
func errFramePayload(ie *InStreamError) []byte {
	payload := map[string]any{
		"error": map[string]any{
			"message": ie.Msg,
			"type":    "upstream_error",
			"code":    ie.Code,
		},
	}
	raw, _ := json.Marshal(payload)
	return raw
}

// errFrameOf 识别错误形状的 chunk：有 code+message 且无 choices 键。
// 正常 chunk（含 usage 尾帧）恒带 choices；错误帧是平台网关的错误对象。
func errFrameOf(chunk map[string]any) (*InStreamError, bool) {
	if _, ok := chunk["choices"]; ok {
		return nil, false
	}
	cv, cok := chunk["code"]
	mv, mok := chunk["message"]
	if !cok || !mok {
		return nil, false
	}
	ie := &InStreamError{}
	switch v := cv.(type) {
	case string:
		ie.Code = v
	case float64:
		ie.Code = strconv.FormatFloat(v, 'f', -1, 64)
	}
	if m, ok := mv.(string); ok {
		ie.Msg = m
	}
	return ie, true
}

// ErrFrameOf 导出别名：qoder 渠道排队协议（peek 首帧判 10605）跨包使用。
func ErrFrameOf(chunk map[string]any) (*InStreamError, bool) { return errFrameOf(chunk) }

// ---------------------------------------------------------------------------
// 首包门（stream head gate）
// ---------------------------------------------------------------------------

// HeadVerdict 首包门判定结果。
type HeadVerdict int

const (
	// HeadAnswered 已拿到内容（可开始向客户端写流）。
	HeadAnswered HeadVerdict = iota
	// HeadFault 拿到错误帧且此前无任何内容（可换号重试，尚未写响应）。
	HeadFault
	// HeadDone 流干净结束但一个字都没答（按失败处理，可换号重试）。
	HeadDone
)

// PrefetchNestedSSE 读到第一个内容帧或错误帧。
//   - HeadAnswered：pre 为已消费字节（含首个内容帧），rest 为剩余流
//   - HeadFault：fault 为错误帧，调用方据此决定换号/冷却
//   - HeadDone：流结束但零内容（空回答按失败处理，可换号重试）
func PrefetchNestedSSE(r io.Reader) (HeadVerdict, []byte, io.Reader, *InStreamError, error) {
	br := bufio.NewReaderSize(r, 256*1024)
	var pre bytes.Buffer
	var event strings.Builder
	for {
		line, rerr := br.ReadString('\n')
		if len(line) > 0 {
			pre.WriteString(line)
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(trimmed, "event:"):
				if event.Len() > 0 {
					event.WriteByte(' ')
				}
				event.WriteString(strings.TrimSpace(strings.TrimPrefix(trimmed, "event:")))
			case strings.HasPrefix(trimmed, "data:"):
				payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
				verdict, ie := headLineVerdict(payload, event.String())
				event.Reset()
				switch verdict {
				case HeadFault:
					return HeadFault, nil, nil, ie, nil
				case HeadDone:
					return HeadDone, nil, nil, nil, nil
				case HeadAnswered:
					// 已读字节（含首个内容帧）原样交回，剩余流继续读。
					return HeadAnswered, pre.Bytes(), br, nil, nil
				}
				// 未定（心跳/role 帧等）：继续读下一行
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return HeadDone, nil, nil, nil, nil
			}
			return HeadDone, nil, nil, nil, rerr
		}
	}
}

// headLineVerdict 判一行 data：内容帧(HeadAnswered) / 错误帧(HeadFault) /
// 结束(HeadDone) / 未定(-1，继续读)。
func headLineVerdict(payload, eventName string) (HeadVerdict, *InStreamError) {
	if strings.EqualFold(strings.TrimSpace(eventName), "finish") {
		return HeadDone, nil
	}
	if payload == "" || payload == "[DONE]" {
		return HeadVerdict(-1), nil
	}
	var env Envelope
	if json.Unmarshal([]byte(payload), &env) != nil {
		return HeadVerdict(-1), nil
	}
	body := strings.TrimSpace(env.Body)
	if body == "" || body == "[DONE]" {
		return HeadVerdict(-1), nil
	}
	if _, _, ok := SplitControlFrame(body); ok {
		return HeadVerdict(-1), nil // 通告帧不是错误
	}
	var chunk map[string]any
	if json.Unmarshal([]byte(body), &chunk) != nil {
		return HeadVerdict(-1), nil
	}
	if ie, ok := errFrameOf(chunk); ok {
		return HeadFault, ie
	}
	// 只有**实际内容**才算 Answer；纯 role 帧不算（否则上游发个 role 帧就
	// 静默的场景会被误判成功，换号机会又被扔掉）。
	if chunkHasPayload(chunk) {
		return HeadAnswered, nil
	}
	return HeadVerdict(-1), nil
}

// chunkHasPayload 帧里是否带实际内容（正文/reasoning/tool_calls/finish_reason）。
func chunkHasPayload(chunk map[string]any) bool {
	chs, _ := chunk["choices"].([]any)
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if fr, ok := c["finish_reason"].(string); ok && fr != "" {
			return true
		}
		d, _ := c["delta"].(map[string]any)
		if d == nil {
			continue
		}
		if s, ok := d["content"].(string); ok && s != "" {
			return true
		}
		if s, ok := d["reasoning_content"].(string); ok && s != "" {
			return true
		}
		if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
			return true
		}
	}
	return false
}

// AggregateNested 聚合完整 SSE 为单个 OpenAI chat.completion 响应。
// model 覆盖响应中的 model 字段；tool_calls 按 index 合并流式 delta。
func AggregateNested(r io.Reader, model string) (map[string]any, error) {
	var (
		id           string
		created      float64
		content      strings.Builder
		reasoning    strings.Builder
		role         = "assistant"
		finishReason = "stop"
		usage        map[string]any
		toolCalls    = map[int]map[string]any{}
		toolOrder    []int
		streamErr    *InStreamError // 错误帧（有内容时降级为部分响应，见下）
	)
	err := ParseNestedSSE(r, func(chunk map[string]any) error {
		if ie, ok := errFrameOf(chunk); ok && streamErr == nil {
			streamErr = ie // 只记首帧，后续帧不覆盖
			return nil
		}
		if v, ok := chunk["id"].(string); ok && id == "" {
			id = v
		}
		if v, ok := chunk["created"].(float64); ok && created == 0 {
			created = v
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		ch, _ := chunk["choices"].([]any)
		for _, ci := range ch {
			c, _ := ci.(map[string]any)
			if c == nil {
				continue
			}
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				finishReason = fr
			}
			delta, _ := c["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			if r2, ok := delta["role"].(string); ok && r2 != "" {
				role = r2
			}
			if txt, ok := delta["content"].(string); ok {
				content.WriteString(txt)
			}
			if rc, ok := delta["reasoning_content"].(string); ok {
				reasoning.WriteString(rc)
			}
			if tcs, ok := delta["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					call, ok := tc.(map[string]any)
					if !ok {
						continue
					}
					idx := 0
					if v, ok := call["index"].(float64); ok {
						idx = int(v)
					}
					merged, seen := toolCalls[idx]
					if !seen {
						merged = map[string]any{"index": idx}
						toolCalls[idx] = merged
						toolOrder = append(toolOrder, idx)
					}
					mergeToolCallDelta(merged, call)
				}
			}
		}
		return nil
	}, nil)
	if err != nil {
		return nil, err
	}
	// 错误帧且未聚合出任何内容：整次请求失败，交由网关按错误处置（轮换/冷却）。
	// 已有内容时降级为部分响应（错误帧多半是收尾通告，丢弃避免白烧已生成内容）。
	if streamErr != nil && content.Len() == 0 && len(toolOrder) == 0 {
		return nil, streamErr
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	message := map[string]any{
		"role":    role,
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sortInts(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	// 部分响应降级（错误帧出现在有内容之后）：把错误挂在顶层 error 字段随响应
	// 下发——非流式客户端读 choices 拿到已生成内容，也能看到上游报了什么。
	if streamErr != nil {
		resp["error"] = map[string]any{
			"message": streamErr.Msg,
			"type":    "upstream_error",
			"code":    streamErr.Code,
		}
	}
	return resp, nil
}

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖（后续分片通常缺省），function.arguments 拼接。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// StreamAsOpenAI 把嵌套 SSE 流转写为标准 OpenAI SSE。
// 每个 chunk 重写 model；末尾补 data: [DONE]。
// 错误帧（code+message、无 choices）不透传：显式写 OpenAI error 事件 + [DONE]，
// 并返回 *InStreamError 让网关处置账号（Busy 类只记日志，其他计入错误）。
func StreamAsOpenAI(w io.Writer, r io.Reader, model string, flush func()) error {
	return streamAsOpenAI(w, r, model, flush)
}

// StreamAsOpenAIPrefixed 同 StreamAsOpenAI，但先原样补写 pre（首包门已消费的
// 原始字节，含首个内容帧）。pre 是**尚未转换**的上游原始 SSE 字节——把它与
// rest 合成一条流再走同一套解析，保证转换口径只有一份。
func StreamAsOpenAIPrefixed(w io.Writer, pre []byte, rest io.Reader, model string, flush func()) error {
	if len(pre) == 0 {
		return streamAsOpenAI(w, rest, model, flush)
	}
	return streamAsOpenAI(w, io.MultiReader(bytes.NewReader(pre), rest), model, flush)
}

func streamAsOpenAI(w io.Writer, r io.Reader, model string, flush func()) error {
	err := ParseNestedSSE(r, func(chunk map[string]any) error {
		if ie, ok := errFrameOf(chunk); ok {
			raw := errFramePayload(ie)
			fmt.Fprintf(w, "data: %s\n\n", raw)
			io.WriteString(w, "data: [DONE]\n\n")
			if flush != nil {
				flush()
			}
			return ie
		}
		chunk["model"] = model
		normalizeDeltaKeys(chunk)
		raw, _ := json.Marshal(chunk)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return err
		}
		if flush != nil {
			flush()
		}
		return nil
	}, nil)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if flush != nil {
		flush()
	}
	return nil
}

// normalizeDeltaKeys 删除空的 content/reasoning_content 键。
// 上游每个 delta 都带两个键（其中一个恒空串），Anthropic 风格客户端会因
// 键集合变化误判内容块类型切换，把一段正文拆成多个块。
func normalizeDeltaKeys(chunk map[string]any) {
	chs, _ := chunk["choices"].([]any)
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		d, _ := c["delta"].(map[string]any)
		if d == nil {
			continue
		}
		if v, ok := d["content"].(string); ok && v == "" {
			delete(d, "content")
		}
		if v, ok := d["reasoning_content"].(string); ok && v == "" {
			delete(d, "reasoning_content")
		}
	}
}

func sortInts(a []int) {
	for i := 0; i < len(a)-1; i++ {
		for j := i + 1; j < len(a); j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}
