// Package session 会话键提取：从请求体派生会话级稳定键（供 ConversationRequestID
// 聚合与轮级复合键派生）。账号粘性路由已整体移除（prompt_cache_key 统一后粘性
// 无缓存收益），本包仅保留键提取与派生逻辑。
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// ExtractKey 从请求体提取会话键；按下列顺序依次尝试，找不到时回退到
// 内容派生的稳定键（见 deriveKey），仍为空则返回空串（绝不失败）。
//  1. metadata.conversation_id
//  2. metadata.conversationId
//  3. conversation_id
//  4. conversationId
//  5. prompt_cache_key（OpenAI 前缀缓存键，见下）
//  6. 派生键：system 提示词 + 首条用户消息的哈希（客户端不发会话 id 时的回退，
//     带 user_id 的请求不派生——见下方契约说明）
//
// 前 5 项均为 conversation 维度（对话级）。metadata.user_id 不作为键
// （P1-anti-monopoly 契约）：user 维度粒度过粗——一个 user 的全部并行对话
// 会被聚合到同一键，且曾抢占顶层 conversation_id 的优先级。
//
// issue #35：客户端实际发 camelCase 的 conversationId，此前只识别 snake_case。
// 现两种命名均识别，snake_case 优先级高于 camelCase（同值不同名命中同一对话时
// 返回相同值，天然不混用）。
//
// 第 5 项 prompt_cache_key：pi-ai 驱动的客户端（dsh 等）把会话 ID 放在这个 OpenAI
// 前缀缓存字段里（而非 conversation_id），网关在 upstream 侧本就认它（见
// InjectPromptCacheKey 优先级 1：客户端自带则原值保留）。
//
// 派生键的 user_id 抑制（P1-anti-monopoly 契约在 fallback 路径的延伸）：body 携带
// metadata.user_id 或顶层 user_id 时**不派生**。ExtractKey 有意剔除 user_id 作键
// （user 维度粒度过粗），若派生路径不设此闸，只发 user_id 的请求会借内容哈希重新
// 获得会话键，使该契约在 fallback 路径失效（上游 #169 同款回归修正）。
func ExtractKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strOrEmpty(meta["conversation_id"]); v != "" {
			return v
		}
		if v := strOrEmpty(meta["conversationId"]); v != "" {
			return v
		}
	}
	if v := strOrEmpty(obj["conversation_id"]); v != "" {
		return v
	}
	if v := strOrEmpty(obj["conversationId"]); v != "" {
		return v
	}
	// 5. prompt_cache_key：OpenAI 系的会话级前缀缓存键，语义就是"同一会话复用同一
	//    前缀"。放显式会话键之后、派生键之前，不抢占优先级。
	if v := strOrEmpty(obj["prompt_cache_key"]); v != "" {
		return v
	}
	// 派生回退闸：带 user 维度标识的请求不派生（契约见上）。
	if hasUserIDKey(obj) {
		return ""
	}
	return deriveKey(obj)
}

// hasUserIDKey 报告已解析 body 是否携带 user 维度标识（metadata.user_id 或顶层
// user_id，非空字符串才算）。只用于派生回退闸（ExtractKey）；解析失败按无处理。
func hasUserIDKey(obj map[string]any) bool {
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if strOrEmpty(meta["user_id"]) != "" {
			return true
		}
	}
	return strOrEmpty(obj["user_id"]) != ""
}

// derivedKeyPrefix 派生键前缀，与显式会话 id 的命名空间隔离：
// 即便客户端恰好传了形如 "d-<hex>" 的显式 id 也不至于与派生键混淆（显式 id 优先返回）。
const derivedKeyPrefix = "d-"

// deriveKey 从消息内容派生稳定会话键：SHA-256(system 文本 + 首条 user 文本) 前 16 字节。
//
// 为什么用「system + 首条 user」而不是全部消息：
//   - 多轮对话里历史消息每轮追加，全量哈希会每轮变化 → 键随轮漂移；
//   - system 与首条 user 在一次对话中恒定，足以区分不同对话。
//
// 取不到用户文本（纯图片等）时返回空串：不聚合，退回轮级键（安全降级）。
func deriveKey(obj map[string]any) string {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return ""
	}
	systemText, firstUserText := "", ""
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		text := messageText(msg["content"])
		switch strOrEmpty(msg["role"]) {
		case "system", "developer":
			if systemText == "" {
				systemText = text
			}
		case "user":
			if firstUserText == "" {
				// 首条 user 用内容签名（ids.go contentSignature）而非纯文本提取：
				// 纯图片轮（无 text part）在 messageText 下恒空串 → 派生键失效
				//（首图会话的盲区）；签名口径下图片 part 以 [type:摘要] 入键。
				firstUserText = userContentSignature(msg["content"])
			}
		}
		if firstUserText != "" && systemText != "" {
			break // 都已拿到：停止遍历长历史
		}
	}
	if firstUserText == "" {
		return "" // 无用户消息：无从归属会话
	}
	sum := sha256.Sum256([]byte(systemText + "\x00" + firstUserText))
	return derivedKeyPrefix + hex.EncodeToString(sum[:16])
}

// userContentSignature 把已解析的 content（any）转回 RawMessage 后取**会话级稳定**
// 签名，供 deriveKey 专用。与 TurnKey 的 contentSignature（全文摘）不同：
// 文本 part 照常拼接；非文本 part 只入 "[type]" 占位**不入内容摘要**——同一逻辑
// 图片的 URL 逐轮变化（签名 URL/重编码 base64）是真实场景，内容摘要入键会让
// 派生键随轮漂移（fork 既有契约：image url changes must not break derived key
// stability）。占位仍能区分"有无图片/图片数量"，纯图片首条消息（无 text part）
// 由此可派生非空键（对齐上游 G1 的目标语义而保留 fork 的稳定性口径）。
// marshal 失败按无内容处理（不伪造）。
func userContentSignature(content any) string {
	raw, err := json.Marshal(content)
	if err != nil {
		return ""
	}
	st := strings.TrimSpace(string(raw))
	if st == "" || st == "null" {
		return ""
	}
	if st[0] == '"' {
		var str string
		if json.Unmarshal(raw, &str) != nil {
			return ""
		}
		return str
	}
	if st[0] != '[' {
		return ""
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	hasNonText := false
	for _, pr := range parts {
		var p struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(pr, &p) != nil {
			return ""
		}
		if p.Type == "" || p.Type == "text" {
			b.WriteString(p.Text)
			continue
		}
		hasNonText = true
		b.WriteString("\n[" + p.Type + "]\n")
	}
	out := b.String()
	if !hasNonText {
		return out
	}
	return strings.TrimSpace(out)
}

// messageText 提取消息 content 的文本表示。
// 兼容：字符串 / [{type:"text",text:"..."}] 数组（OpenAI 多模态）；其他类型取空。
func messageText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, part := range v {
			if p, ok := part.(map[string]any); ok {
				sb.WriteString(strOrEmpty(p["text"]))
			}
		}
		return sb.String()
	}
	return ""
}

// strOrEmpty 把 JSON 字符串字段安全转 string（非字符串类型返回空）。
func strOrEmpty(v any) string {
	s, _ := v.(string)
	return s
}
