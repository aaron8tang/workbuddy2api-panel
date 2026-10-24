// body.go 构造千问办公 agent_chat_generation 请求体。
// 与 qoderwork 同族（同一 CLI 代码库），差异：
//   - session_type 从 Profile 取（qodercli，CN CLI 口径）
//   - aliyun_user_type：千问办公 JWT 身份无账号类别，不发该字段
//   - business.product=cli（进 headers；body 不再重复 business 块）
package qwenwork

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cosy"
)

// IsReasoningModel 按思考链路构造（is_reasoning=true + source="system"）。
func IsReasoningModel(modelKey string) bool {
	return modelKey != ""
}

// NormalizeReasoningEffort 校验思考档位（族内 parameters 支持 low/medium/xhigh）。
// 非法值返回空串 = 不注入 parameters。
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

// BuildAgentBody 构造请求体（未编码）。
func BuildAgentBody(openaiMessages []map[string]any, modelKey string, clientTools []any, reasoningEffort string) ([]byte, error) {
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
		"request_id":     newUUID,
		"chat_record_id": newUUID,
		"request_set_id": cosy.UUID(),
		"session_id":     cosy.UUID(),
		"stream":         true,
		"agent_id":       "agent_common",
		"chat_task":      "FREE_INPUT",
		"is_reply":       true,
		"image_urls":     nil,
		"session_type":   cosy.ProfileQwenWork().SessionType,
		"model_config":   modelConfig,
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

	if len(clientTools) > 0 {
		base["tools"] = clientTools
	}
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
