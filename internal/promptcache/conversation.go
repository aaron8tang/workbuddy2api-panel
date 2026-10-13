// conversation.go 跨池统一的会话标识解析链（cache key 的会话源）。
//
// 背景：SOLO（trae/chat.go extractSessionKey）与 CN 主池
//（session.ResolveConversationID + handler 粘性键）原本各自一套口径：
//   - CN 只认 body 四级（metadata 双形态 → 顶层双形态），缺失返回 ""；
//   - SOLO 三级（body 双形态 → 头族 → 指纹）。
//
// cache key 去账号化后，会话源成为 key 的唯一变量，两池必须用同一条
// 解析链，否则同一请求走不同池会得出不同 key。统一链（优先级从高到低）：
//  1. body metadata.conversation_id / metadata.conversationId
//  2. body conversation_id / conversationId（顶层）
//  3. HTTP 头 X-Session-ID / X-Conversation-ID / Session-Id
//  4. body user 字段
//  5. 消息前缀特征指纹 fp:sha256[:8]（system 前缀 + 首条非 system 消息）
//
// 注意：必须在 prompt.Rewrite 等改写之前的**原始 body** 上计算（蓝本对齐
// trae/chat.go：convID 在 setModelInBody/改写之前 peek；CN handler 的
// 组裁决只动顶层字段不动 messages，Rewrite 前的 body 对本链等价原始）。
package promptcache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// ResolveConversation 从原始请求解析会话标识（统一链，见文件注）。
// header 可为 nil（无 HTTP 头上下文的调用方）。全部源缺失返回 ""。
func ResolveConversation(header http.Header, body []byte) string {
	if conv := resolveFromBody(body); conv != "" {
		return conv
	}
	if header != nil {
		if sid := strings.TrimSpace(header.Get("X-Session-ID")); sid != "" {
			return sid
		}
		if cid := strings.TrimSpace(header.Get("X-Conversation-ID")); cid != "" {
			return cid
		}
		if sid := strings.TrimSpace(header.Get("Session-Id")); sid != "" {
			return sid
		}
	}
	return fingerprint(body)
}

// resolveFromBody body 四级提取（metadata 双形态 → 顶层双形态）+ user
// 字段。metadata/顶层与 session.ResolveConversationID 同口径。
func resolveFromBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strField(meta, "conversation_id"); v != "" {
			return v
		}
		if v := strField(meta, "conversationId"); v != "" {
			return v
		}
	}
	if v := strField(obj, "conversation_id"); v != "" {
		return v
	}
	if v := strField(obj, "conversationId"); v != "" {
		return v
	}
	if v := strField(obj, "user"); v != "" {
		return v
	}
	return ""
}

// fingerprint 多轮对话前缀特征指纹（蓝本对齐 trae/chat.go
// extractSessionKey 末级）：system 前缀 + 首条非 system 消息的
// role+content 做 SHA-256，返回 "fp:"+hex[:8]。无 messages 返回 ""。
func fingerprint(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		return ""
	}
	h := sha256.New()
	if req.Messages[0].Role == "system" {
		h.Write([]byte("sys:"))
		h.Write(req.Messages[0].Content)
	}
	for _, m := range req.Messages {
		if m.Role != "system" {
			h.Write([]byte(m.Role + ":"))
			h.Write(m.Content)
			break
		}
	}
	return "fp:" + hex.EncodeToString(h.Sum(nil)[:8])
}
