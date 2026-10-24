// body.go 构造 agent_chat_generation 请求体（纯透传模式）。
// 移植自 qoderwork2api internal/upstream/body.go：客户端消息全量转发
//（含 system/assistant/tool 多轮），tools 仅在客户端显式传入时注入。
package qoderwork

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cosy"
)

// IsReasoningModel 判断是否按思考链路构造（is_reasoning=true + source="system"）。
// 上游对不支持的模型忽略该字段；source="system" 是触发思考下发的真正开关。
func IsReasoningModel(modelKey string) bool {
	return modelKey != ""
}

// NormalizeReasoningEffort 校验思考档位（上游 parameters 支持 low/medium/xhigh）。
// 非法值返回空串 = 不注入 parameters，走上游默认（medium）。
func NormalizeReasoningEffort(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "low":
		return "low"
	case "medium":
		return "medium"
	case "xhigh":
		return "xhigh"
	default:
		return ""
	}
}

// BuildAgentBody 构造请求体。
//   - openaiMessages：客户端原始消息列表（可含 system/assistant/tool 多轮）
//   - modelKey：上游模型 key（如 qmodel_preview）
//   - clientTools：客户端传来的 OpenAI tools 数组；为空则不注入 tools 字段
//   - reasoningEffort：思考档位（low/medium/xhigh）；空 = 上游默认
func BuildAgentBody(openaiMessages []map[string]any, modelKey string, clientTools []any, reasoningEffort string) ([]byte, error) {
	// 最后一条 user 消息文本（chat_context.text 上游协议要求必填）
	prompt := ""
	for i := len(openaiMessages) - 1; i >= 0; i-- {
		if openaiMessages[i]["role"] == "user" {
			if c, ok := openaiMessages[i]["content"].(string); ok && c != "" {
				prompt = c
				break
			}
		}
	}

	now := time.Now()
	newUUID := cosy.UUID()

	isReasoning := IsReasoningModel(modelKey)
	modelConfig := map[string]any{"key": modelKey, "is_reasoning": isReasoning}
	if isReasoning {
		modelConfig["source"] = "system"
	}

	base := map[string]any{
		"request_id":       newUUID,
		"chat_record_id":   newUUID,
		"request_set_id":   cosy.UUID(),
		"session_id":       cosy.UUID(),
		"stream":           true,
		"aliyun_user_type": "personal_professional_trial",
		"agent_id":         "agent_common",
		"chat_task":        "FREE_INPUT",
		"is_reply":         true,
		"image_urls":       nil,
		"session_type":     "qodercli",
		"model_config":     modelConfig,
		"chat_context": map[string]any{
			"chatPrompt": "",
			"text":       map[string]any{"type": "text", "text": prompt},
			"extra": map[string]any{
				"context":         []any{},
				"modelConfig":     map[string]any{"key": modelKey, "is_reasoning": isReasoning},
				"originalContent": map[string]any{"type": "text", "text": prompt},
			},
			"features":  []any{},
			"imageUrls": nil,
		},
		"messages": openaiMessages,
		"business": map[string]any{
			"id":       cosy.UUID(),
			"begin_at": now.UnixMilli(),
			"name":     truncateRunes(prompt, 30),
		},
	}

	// tools：客户端传了才注入
	if len(clientTools) > 0 {
		base["tools"] = clientTools
	}

	// 思考档位：客户端显式传了合法值时注入；不传 = 上游默认（medium）。
	if isReasoning && reasoningEffort != "" {
		base["parameters"] = map[string]any{
			"enable_thinking":  true,
			"reasoning_effort": reasoningEffort,
		}
	}

	return json.Marshal(base)
}

// truncateRunes 截断到 n 个 rune。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
