// chat.go AutoClaw 裸 OpenAI SSE 首包门 + 流透传 + 非流式聚合。
// 上游是标准 OpenAI SSE（非嵌套 envelope）——帧直接是 chat.completion.chunk；
// 上游把每帧 model 回写成自己的路由/内部名，网关必须逐帧回写成客户端请求的原名。
package autoclaw

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// InStreamErr 流内错误帧（HTTP 200 SSE 里的 {"error":{...}} 或裸 {code,message}）。
type InStreamErr struct {
	Code    string
	Message string
}

func (e *InStreamErr) Error() string {
	return fmt.Sprintf("upstream in-stream error code=%s: %s", e.Code, e.Message)
}

// headVerdict 首包门判定。
type headVerdict int

const (
	headAnswered headVerdict = iota
	headFault
	headDone
)

// prefetchOpenAISSE 裸 OpenAI SSE 首包门：读到第一个内容帧/错误帧。
// 返回 (verdict, pre 已消费原始字节, rest 剩余流, 错误帧, 读错误)。
func prefetchOpenAISSE(r io.Reader) (headVerdict, []byte, io.Reader, *InStreamErr, error) {
	br := bufio.NewReaderSize(r, 256*1024)
	var pre bytes.Buffer
	for {
		line, rerr := br.ReadString('\n')
		if len(line) > 0 {
			pre.WriteString(line)
			if payload, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "data:"); ok {
				payload = strings.TrimSpace(payload)
				verdict, ie := openAIDataVerdict(payload)
				switch verdict {
				case headFault:
					return headFault, nil, nil, ie, nil
				case headDone:
					return headDone, nil, nil, nil, nil
				case headAnswered:
					return headAnswered, pre.Bytes(), br, nil, nil
				}
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return headDone, nil, nil, nil, nil
			}
			return headDone, nil, nil, nil, rerr
		}
	}
}

// openAIDataVerdict 判一行 data（标准 OpenAI SSE）。
func openAIDataVerdict(payload string) (headVerdict, *InStreamErr) {
	if payload == "" || payload == "[DONE]" {
		return headDone, nil
	}
	var chunk map[string]any
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return -1, nil // 非 JSON：继续读
	}
	// 错误形态 1：{"error":{...}}。
	if e, ok := chunk["error"].(map[string]any); ok {
		ie := &InStreamErr{}
		if s, ok := e["code"].(string); ok {
			ie.Code = s
		} else if f, ok := e["code"].(float64); ok {
			ie.Code = fmt.Sprintf("%g", f)
		}
		if s, ok := e["message"].(string); ok {
			ie.Message = s
		}
		if ie.Message == "" {
			ie.Message = "upstream error"
		}
		return headFault, ie
	}
	// 错误形态 2：裸 {code, message} 无 choices（对齐 cosy errFrameOf 口径）。
	if _, hasChoices := chunk["choices"]; !hasChoices {
		cv, cok := chunk["code"]
		mv, mok := chunk["message"]
		if cok && mok {
			ie := &InStreamErr{}
			switch v := cv.(type) {
			case string:
				ie.Code = v
			case float64:
				ie.Code = fmt.Sprintf("%g", v)
			}
			if s, ok := mv.(string); ok {
				ie.Message = s
			}
			return headFault, ie
		}
	}
	// 内容判定（纯 role 帧不算答案——与 cosy chunkHasPayload 同口径）。
	if openAIHasPayload(chunk) {
		return headAnswered, nil
	}
	return -1, nil
}

// openAIHasPayload 帧里是否带实际内容。
func openAIHasPayload(chunk map[string]any) bool {
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

// streamOpenAIPrefixed 把 pre（已消费原始字节，含首个内容帧）+ rest 转写为标准
// OpenAI SSE（逐帧回写 model；末尾补 [DONE]）。
func streamOpenAIPrefixed(w io.Writer, pre []byte, rest io.Reader, model string, flush func()) error {
	var r io.Reader = rest
	if len(pre) > 0 {
		r = io.MultiReader(bytes.NewReader(pre), rest)
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		line := scanner.Text()
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if _, isErr := chunk["error"]; isErr {
			// 内容已写出后的错误帧：按 OpenAI error 事件透传（响应已 200）。
			raw, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", raw)
			fmt.Fprint(w, "data: [DONE]\n\n")
			if flush != nil {
				flush()
			}
			return fmt.Errorf("upstream in-stream error (mid-stream)")
		}
		// 纯换行节拍帧（带 role 无内容）丢弃——本家特有形态（adapter.rs:446-456）。
		if !openAIHasPayload(chunk) && !hasRole(chunk) {
			continue
		}
		chunk["model"] = model
		raw, _ := json.Marshal(chunk)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return err
		}
		if flush != nil {
			flush()
		}
	}
	_, err := fmt.Fprint(w, "data: [DONE]\n\n")
	if flush != nil {
		flush()
	}
	return err
}

// hasRole 帧是否带 role（纯 role 起始帧不透传——无内容无意义）。
func hasRole(chunk map[string]any) bool {
	chs, _ := chunk["choices"].([]any)
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if d, ok := c["delta"].(map[string]any); ok {
			if _, has := d["role"]; has {
				return true
			}
		}
	}
	return false
}

// aggregateOpenAI 非流式聚合（SSE → 一条 completion）。
// 返回 (体, fault)；fault 为 *InStreamErr 时调用方可换号重试。
func aggregateOpenAI(r io.Reader, model string) ([]byte, error) {
	var content, reasoning strings.Builder
	type toolCall struct {
		Index    int    `json:"index"`
		ID       string `json:"id,omitempty"`
		Type     string `json:"type,omitempty"`
		Function struct {
			Name      string `json:"name,omitempty"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	tools := map[int]*toolCall{}
	var toolOrder []int
	var usage map[string]any
	var finish string
	var chatID string
	var streamErr *InStreamErr

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		payload, ok := strings.CutPrefix(scanner.Text(), "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if _, isErr := chunk["error"]; isErr {
			if streamErr == nil { // 只记首帧
				if raw, err := json.Marshal(chunk["error"]); err == nil {
					var e map[string]any
					_ = json.Unmarshal(raw, &e)
					ie := &InStreamErr{Message: "upstream error"}
					if s, ok := e["code"].(string); ok {
						ie.Code = s
					}
					if s, ok := e["message"].(string); ok {
						ie.Message = s
					}
					streamErr = ie
				}
			}
			continue
		}
		if v, ok := chunk["id"].(string); ok && v != "" && chatID == "" {
			chatID = v
		}
		if u, ok := chunk["usage"].(map[string]any); ok && u != nil {
			usage = u
		}
		chs, _ := chunk["choices"].([]any)
		for _, ci := range chs {
			c, _ := ci.(map[string]any)
			if c == nil {
				continue
			}
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				finish = fr
			}
			d, _ := c["delta"].(map[string]any)
			if d == nil {
				continue
			}
			if s, ok := d["content"].(string); ok {
				content.WriteString(s)
			}
			if s, ok := d["reasoning_content"].(string); ok {
				reasoning.WriteString(s)
			}
			if tcs, ok := d["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					m, _ := tc.(map[string]any)
					if m == nil {
						continue
					}
					idx := 0
					if v, ok := m["index"].(float64); ok {
						idx = int(v)
					}
					slot := tools[idx]
					if slot == nil {
						slot = &toolCall{Index: idx}
						tools[idx] = slot
						toolOrder = append(toolOrder, idx)
					}
					if s, ok := m["id"].(string); ok && s != "" && slot.ID == "" {
						slot.ID = s
					}
					if fn, ok := m["function"].(map[string]any); ok {
						if s, ok := fn["name"].(string); ok && s != "" && slot.Function.Name == "" {
							slot.Function.Name = s
						}
						if s, ok := fn["arguments"].(string); ok {
							slot.Function.Arguments += s
						}
					}
				}
			}
		}
	}
	hasContent := content.Len() > 0 || reasoning.Len() > 0 || len(toolOrder) > 0
	if !hasContent {
		if streamErr != nil {
			return nil, streamErr
		}
		return nil, fmt.Errorf("upstream stream ended with no content")
	}
	if finish == "" {
		if len(toolOrder) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	if chatID == "" {
		chatID = "chatcmpl-autoclaw-" + newRequestID()[:12]
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
	out := map[string]any{
		"id": chatID, "object": "chat.completion",
		"created": timeNow().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
	}
	if usage != nil {
		out["usage"] = usage
	}
	return json.Marshal(out)
}
