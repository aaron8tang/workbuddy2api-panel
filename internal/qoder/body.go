// body.go Qoder(qoder.sh) chat 请求体构造与消息映射。
// 移植自 API-Console internal/qoder/{request,request_history}.go：
//   - system 消息折叠到顶层 system 字段（网关两处都读）
//   - tool 结果必须保留为 tool 消息（改写为文本会被判 malformed）
//   - tools 为 nil 时必须置 []any{}（部分套餐网关拒绝 null）
//   - chat_context 的 text/originalContent 是纯字符串（QoderWork 侧才是对象）
//   - business.name = 最新 user 文本前 10 rune
//   - 会话粘性会话 ID 由 (uid, conversation) 哈希派生，不落原始 ID
package qoder

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cosy"
)

const (
	chatTask    = "FREE_INPUT"
	sourceValue = 1
	taskID      = "common"
	agentID     = "agent_common"
	subTask     = "ws_builtin_general"
	bodyVersion = "3"
)

// modelEntry 上游模型条目（/algo/api/v2/model/list chat 场景行）。
type modelEntry struct {
	Key            string   `json:"key"`
	Name           string   `json:"name"`
	DisplayName    string   `json:"display_name"`
	Format         string   `json:"format"`
	Source         string   `json:"source"`
	Enable         *bool    `json:"enable"`
	IsVL           bool     `json:"is_vl"`
	IsReasoning    bool     `json:"is_reasoning"`
	IsDefault      bool     `json:"is_default"`
	PriceFactor    *float64 `json:"price_factor,omitempty"`
	MaxInputTokens int      `json:"max_input_tokens"`
}

func (m modelEntry) enabled() bool { return m.Enable == nil || *m.Enable }

// modelConfigWire model_config 完整线格式（参考桥模板键位）。
type modelConfigWire struct {
	Key            string `json:"key"`
	DisplayName    string `json:"display_name"`
	Model          string `json:"model"`
	Format         string `json:"format"`
	IsVL           bool   `json:"is_vl"`
	IsReasoning    bool   `json:"is_reasoning"`
	APIKey         string `json:"api_key"`
	URL            string `json:"url"`
	Source         string `json:"source"`
	MaxInputTokens int    `json:"max_input_tokens"`
}

func wireModelConfig(m modelEntry) modelConfigWire {
	format := m.Format
	if strings.TrimSpace(format) == "" {
		format = "openai"
	}
	source := m.Source
	if strings.TrimSpace(source) == "" {
		source = "system"
	}
	name := m.DisplayName
	if name == "" {
		name = m.Name
	}
	if name == "" {
		name = m.Key
	}
	return modelConfigWire{
		Key:            m.Key,
		DisplayName:    name,
		Format:         format,
		Source:         source,
		IsVL:           m.IsVL,
		IsReasoning:    m.IsReasoning,
		MaxInputTokens: m.MaxInputTokens,
	}
}

// chatBody 请求体（字段序仅为可读性；签名覆盖的是编码后字节）。
type chatBody struct {
	Business          businessInfo    `json:"business"`
	RequestID         string          `json:"request_id"`
	RequestSetID      string          `json:"request_set_id"`
	ChatRecordID      string          `json:"chat_record_id"`
	SessionID         string          `json:"session_id"`
	Stream            bool            `json:"stream"`
	ChatTask          string          `json:"chat_task"`
	ChatContext       any             `json:"chat_context"`
	IsReply           bool            `json:"is_reply"`
	IsRetry           bool            `json:"is_retry"`
	Source            int             `json:"source"`
	Version           string          `json:"version"`
	AgentID           string          `json:"agent_id"`
	TaskID            string          `json:"task_id"`
	SessionType       string          `json:"session_type"`
	AliyunUser        string          `json:"aliyun_user_type"`
	ModelConfig       modelConfigWire `json:"model_config"`
	System            string          `json:"system"`
	Messages          []chatMessage   `json:"messages"`
	Tools             []any           `json:"tools"`
	ToolChoice        any             `json:"tool_choice,omitempty"`
	Parameters        chatParameters  `json:"parameters"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
}

// businessInfo 遥测块。business.id 是任务集 id（= requestSetID），非请求 id。
type businessInfo struct {
	Product string `json:"product"`
	Version string `json:"version"`
	Type    string `json:"type"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	BeginAt int64  `json:"begin_at"`
	Stage   string `json:"stage"`
	SubTask string `json:"sub_task"`
}

// chatMessage 上游历史单条。
type chatMessage struct {
	Role          string          `json:"role"`
	Content       string          `json:"content,omitempty"`
	Contents      []chatPart      `json:"contents,omitempty"`
	ToolCalls     []chatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID    string          `json:"tool_call_id,omitempty"`
	Name          string          `json:"name,omitempty"`
	Reasoning     string          `json:"reasoning_content,omitempty"`
	Meta          json.RawMessage `json:"response_meta,omitempty"`
	ReasoningItem json.RawMessage `json:"reasoning_item,omitempty"`
}

// chatPart 多模态 user 消息内容部件。
type chatPart struct {
	Type     string         `json:"type"`
	Text     string         `json:"text,omitempty"`
	ImageURL *chatImageURL  `json:"image_url,omitempty"`
	Source   map[string]any `json:"source,omitempty"`
}

type chatImageURL struct {
	URL string `json:"url"`
}

// chatToolCall 历史中的 OpenAI tool-call 形态。
type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chatParameters 参数块。max_tokens 缺省 32000（参考桥输出预算）。
type chatParameters struct {
	ResponseFormat  map[string]any `json:"response_format,omitempty"`
	MaxTokens       int            `json:"max_tokens"`
	Temperature     *float64       `json:"temperature,omitempty"`
	TopP            *float64       `json:"top_p,omitempty"`
	Stop            []string       `json:"stop,omitempty"`
	ContextLength   int            `json:"context_length,omitempty"`
	EnableThinking  *bool          `json:"enable_thinking,omitempty"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
}

// referenceContext 轻量上下文元数据（text/originalContent 均为纯字符串）。
type referenceContext struct {
	ChatPrompt string `json:"chatPrompt"`
	Extra      struct {
		Context         []any  `json:"context"`
		ModelConfig     any    `json:"modelConfig"`
		OriginalContent string `json:"originalContent"`
	} `json:"extra"`
	Features  []any  `json:"features"`
	ImageURLs any    `json:"imageUrls"`
	Text      string `json:"text"`
}

// ChatOptions BuildChatBody 的输入。
type ChatOptions struct {
	Messages          []map[string]any
	Tools             []any
	ToolChoice        any
	ParallelToolCalls *bool
	Temperature       *float64
	TopP              *float64
	MaxTokens         int
	Stop              []string
	ResponseFormat    map[string]any
	ReasoningEffort   string

	Model           modelEntry
	SessionID       string
	RequestID       string
	RequestSetID    string
	BusinessProduct string
	ClientVersion   string
}

// BuildChatBody 构造并序列化请求体（未编码；编码由 upstream.EncodeBody 完成）。
func BuildChatBody(o ChatOptions) ([]byte, error) {
	messages, systemText := buildMessages(o.Messages)

	parameters := chatParameters{MaxTokens: 32000, Temperature: o.Temperature, TopP: o.TopP}
	parameters.ResponseFormat = o.ResponseFormat
	if o.MaxTokens > 0 {
		parameters.MaxTokens = o.MaxTokens
	}
	if len(o.Stop) > 0 {
		parameters.Stop = append([]string{}, o.Stop...)
	}
	if o.Model.MaxInputTokens > 0 {
		parameters.ContextLength = o.Model.MaxInputTokens
	}
	// 参考网关对 is_reasoning 目录行也默认关思考；仅显式传档位时打开。
	effort := strings.ToLower(strings.TrimSpace(o.ReasoningEffort))
	if effort != "" && effort != "none" {
		thinking := true
		parameters.EnableThinking = &thinking
		parameters.ReasoningEffort = effort
	} else if effort == "none" {
		thinking := false
		parameters.EnableThinking = &thinking
	}

	tools := o.Tools
	if tools == nil {
		tools = []any{} // 网关拒绝 null tools
	}

	product := o.BusinessProduct
	if product == "" {
		product = "qoder_work"
	}
	version := o.ClientVersion

	body := chatBody{
		Business: businessInfo{
			Product: product,
			Version: version,
			Type:    "agent",
			ID:      o.RequestSetID,
			Name:    businessName(o.Messages),
			BeginAt: time.Now().UnixMilli(),
			Stage:   "start",
			SubTask: subTask,
		},
		RequestID:    o.RequestID,
		RequestSetID: o.RequestSetID,
		ChatRecordID: o.RequestID,
		SessionID:    o.SessionID,
		Stream:       true,
		ChatTask:     chatTask,
		ChatContext:  referenceChatContext(o.Messages, o.Model),
		IsReply:      true,
		Source:       sourceValue,
		Version:      bodyVersion,
		AgentID:      agentID,
		TaskID:       taskID,
		SessionType:  "qoder_work",
		// QoderWork 国际客户端实测 aliyun_user_type 为空串
		ModelConfig: wireModelConfig(o.Model),
		System:      systemText,
		Messages:    messages,
		Tools:       tools,
		ToolChoice:  o.ToolChoice,
		Parameters:  parameters,
	}
	if o.ParallelToolCalls != nil && len(tools) > 0 {
		body.ParallelToolCalls = o.ParallelToolCalls
	}
	return json.Marshal(body)
}

func referenceChatContext(messages []map[string]any, model modelEntry) referenceContext {
	prompt := latestUserText(messages)
	ctx := referenceContext{Features: []any{}, Text: prompt}
	ctx.Extra.Context = []any{}
	ctx.Extra.ModelConfig = map[string]any{
		"is_reasoning": model.IsReasoning,
		"key":          model.Key,
	}
	ctx.Extra.OriginalContent = prompt
	return ctx
}

// buildMessages 渲染历史：system 折叠到首条 system 消息并单独返回顶层字段；
// 孤儿 tool 结果剔除（上游拒绝悬空配对）。
func buildMessages(messages []map[string]any) ([]chatMessage, string) {
	var systemParts []string
	out := make([]chatMessage, 0, len(messages)+1)

	toolCallIDs := map[string]bool{}
	for _, msg := range messages {
		role := strings.ToLower(strings.TrimSpace(str(msg, "role")))
		if role == "developer" {
			role = "system" // 改名后的 system 角色，折叠保留
		}
		// tool 角色最先分流：必须保留为 tool 消息（改写为文本会被上游判 malformed），
		// 悬空 tool 结果（无对应 tool_call_id）剔除。
		if role == "tool" {
			toolID := strings.TrimSpace(str(msg, "tool_call_id"))
			if toolID == "" || !toolCallIDs[toolID] {
				continue
			}
			delete(toolCallIDs, toolID)
			out = append(out, chatMessage{
				Role:       "tool",
				ToolCallID: toolID,
				Content:    stringContent(msg["content"]),
			})
			continue
		}
		content := msg["content"]
		if role == "system" {
			if text := stringContent(content); strings.TrimSpace(text) != "" {
				systemParts = append(systemParts, strings.TrimSpace(text))
				continue
			}
			// system 带块内容时按普通消息渲染
		}

		if text := stringContent(content); content != nil && text != "" {
			m := chatMessage{Role: normalRole(role), Content: text}
			if role == "assistant" {
				m.Reasoning = str(msg, "reasoning_content")
			}
			if calls, ok := msg["tool_calls"].([]any); ok && len(calls) > 0 {
				m.ToolCalls = convertToolCalls(calls, toolCallIDs)
			}
			out = append(out, m)
			continue
		}

		// 无字符串 content：assistant 空文本（仅 reasoning/tool_calls）或块列表
		switch role {
		case "assistant":
			m := chatMessage{Role: "assistant", Reasoning: str(msg, "reasoning_content")}
			if calls, ok := msg["tool_calls"].([]any); ok && len(calls) > 0 {
				m.ToolCalls = convertToolCalls(calls, toolCallIDs)
			}
			// 块形 content（text/thinking 混排）
			var texts []string
			for _, block := range blockList(content) {
				bt := str(block, "type")
				switch bt {
				case "text":
					if t := str(block, "text"); strings.TrimSpace(t) != "" {
						texts = append(texts, t)
					}
				case "thinking":
					if m.Reasoning == "" {
						m.Reasoning = str(block, "thinking")
					}
				case "tool_use":
					name := strings.TrimSpace(str(block, "name"))
					if name == "" {
						continue
					}
					id := strings.TrimSpace(str(block, "id"))
					if id == "" {
						id = cosy.UUID()
					}
					args, _ := json.Marshal(block["input"])
					call := chatToolCall{ID: id, Type: "function"}
					call.Function.Name = name
					call.Function.Arguments = compactJSON(args)
					m.ToolCalls = append(m.ToolCalls, call)
					toolCallIDs[id] = true
				}
			}
			m.Content = strings.Join(texts, "\n")
			if m.Content == "" && len(m.ToolCalls) == 0 && m.Reasoning == "" {
				continue
			}
			out = append(out, m)
		default:
			// user/system 块内容；tool_result 拆独立 tool 消息
			for _, block := range blockList(content) {
				bt := str(block, "type")
				switch bt {
				case "text":
					if t := str(block, "text"); strings.TrimSpace(t) != "" {
						out = append(out, chatMessage{Role: normalRole(role), Content: t})
					}
				case "image_url":
					if img, ok := block["image_url"].(map[string]any); ok {
						if url := strings.TrimSpace(str(img, "url")); url != "" {
							out = append(out, chatMessage{
								Role:     normalRole(role),
								Contents: []chatPart{{Type: "image_url", ImageURL: &chatImageURL{URL: url}}},
							})
						}
					}
				case "tool_result":
					toolID := strings.TrimSpace(str(block, "tool_call_id"))
					if toolID == "" {
						toolID = strings.TrimSpace(str(block, "tool_use_id"))
					}
					if toolID == "" || !toolCallIDs[toolID] {
						continue
					}
					delete(toolCallIDs, toolID)
					result, _ := json.Marshal(block["content"])
					out = append(out, chatMessage{
						Role:       "tool",
						ToolCallID: toolID,
						Content:    stringContent(json.RawMessage(result)),
					})
				}
			}
		}
	}

	systemText := strings.Join(systemParts, "\n")
	if systemText != "" {
		out = append([]chatMessage{{Role: "system", Content: systemText}}, out...)
	}
	if len(out) == 0 {
		out = append(out, chatMessage{Role: "user", Content: "Hello"})
	}
	return out, systemText
}

func convertToolCalls(calls []any, toolCallIDs map[string]bool) []chatToolCall {
	out := make([]chatToolCall, 0, len(calls))
	for _, c := range calls {
		call, ok := c.(map[string]any)
		if !ok {
			continue
		}
		id := strings.TrimSpace(str(call, "id"))
		if id == "" {
			id = cosy.UUID()
		}
		entry := chatToolCall{ID: id, Type: "function"}
		if t := str(call, "type"); t != "" {
			entry.Type = t
		}
		if fn, ok := call["function"].(map[string]any); ok {
			entry.Function.Name = str(fn, "name")
			args := str(fn, "arguments")
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			entry.Function.Arguments = args
		}
		out = append(out, entry)
		toolCallIDs[id] = true
	}
	return out
}

// businessName 最新 user 文本前 10 rune（上游会话标签，必须有界）。
func businessName(messages []map[string]any) string {
	text := strings.TrimSpace(latestUserText(messages))
	runes := []rune(text)
	if len(runes) > 10 {
		runes = runes[:10]
	}
	return string(runes)
}

// latestUserText 最新一条 user 消息的文本。
func latestUserText(messages []map[string]any) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if !strings.EqualFold(strings.TrimSpace(str(msg, "role")), "user") {
			continue
		}
		if text := strings.TrimSpace(stringContent(msg["content"])); text != "" {
			return text
		}
		for _, block := range blockList(msg["content"]) {
			if str(block, "type") == "text" && strings.TrimSpace(str(block, "text")) != "" {
				return str(block, "text")
			}
		}
	}
	return ""
}

// conversationSessionID 把显式会话身份哈希成账号内稳定的 UUID（不落原始 ID）。
func conversationSessionID(uid, conversation string) string {
	sum := sha256.Sum256([]byte("qoder-session\x00" + uid + "\x00" + conversation))
	sum[6] = (sum[6] & 0x0f) | 0x80
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// ---------------------------------------------------------------------------
// map 消息小工具
// ---------------------------------------------------------------------------

func normalRole(role string) string {
	switch role {
	case "assistant", "system", "tool":
		return role
	default:
		return "user"
	}
}

func str(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return v
	case json.RawMessage:
		return string(v)
	default:
		return ""
	}
}

// stringContent 把 OpenAI content（string 或块数组）折叠为纯文本。
func stringContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, b := range v {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if t := strings.TrimSpace(str(block, "text")); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func blockList(content any) []map[string]any {
	blocks, ok := content.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		if m, ok := b.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// compactJSON 压缩 JSON（去空白）；非法 JSON 原样返回。
func compactJSON(raw []byte) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}
