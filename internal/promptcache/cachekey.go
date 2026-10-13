// Package promptcache 提供 prompt_cache_key 注入（P0）与两段式会话压缩
// （P1）的跨池共享实现。
//
// 背景：网关内有两条平行的上游通道——SOLO（internal/trae/upstream，打
// trae-api-cn.mchost.guru）与 CN 主池（internal/upstream，打
// copilot.tencent.com）。同一调用工具（如 WorkBuddy / TraeCN）解决同一
// 问题时，账号会在两池间轮询使用：池内换号接力、跨池往返切换都常见。
// 若 cache key 绑定账号 UID，任何一次换号都会让上游前缀缓存失配
// （key 变了 → 上游按新 key 建缓存 → 旧前缀全部重算），P0 的 ~17×
// 费用优化形同虚设。
//
// 因此本包的 cache key 是纯会话化的：
//
//	wb2a-<sha256(conversation)[:16] hex>
//
// 同一会话无论用哪个账号、走哪个池，key 恒定。
//
// 安全性说明（取代旧的账号隔离设计）：
//   - 上游前缀缓存命中仍要求出站前缀字节一致——持有 cache key 本身
//     不能读到任何缓存内容，无跨账号信息泄露面；
//   - 两池打不同上游服务，物理缓存各自独立，跨池共享的只是 key 的
//     稳定性（往返时各自池内的历史缓存仍可命中）；
//   - summaryCache（P1 裁剪状态）按通道分桶，因为两池的 PrepareBody
//     改写语义不同，跨池指纹必然失配，不分桶会互相覆盖、反复重算。
package promptcache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// InjectPromptCacheKey 在已改写的出站 body 上注入 prompt_cache_key 字段。
//
// 优先级：
//  1. body 已带 prompt_cache_key → 原值保留（客户端自知复用哪个键）
//  2. body 已带 conversation_id / conversationId → 用它做会话哈希源
//  3. 都没有 → 用入参 conversationID（编排层经 ResolveConversation
//     从原始 body/请求头解析后传入；SOLO 侧在 PrepareBody 之后调用，
//     因白名单会剔除 body 内 conversation_id，参数是主路径）
//
// 会话源为空时 convHex 为定值（无正确性问题，仅单桶缓存复用；统一
// 解析链 ResolveConversation 让空源罕见）。body 不可解析时原样返回
// （与两池 PrepareBody 语义一致：坏 body 不二次错误化）。
func InjectPromptCacheKey(body []byte, conversationID string) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	// 优先级 1：客户端已显式带 key → 绝不覆盖。
	if existing, ok := obj["prompt_cache_key"].(string); ok && existing != "" {
		return body
	}
	// 优先级 2：body 里的 conversation_id / conversationId 做会话哈希源。
	conv := conversationID
	if v := strField(obj, "conversation_id"); v != "" {
		conv = v
	} else if v := strField(obj, "conversationId"); v != "" {
		conv = v
	}
	obj["prompt_cache_key"] = BuildCacheKey(conv)
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// BuildCacheKey 生成 `wb2a-<convHex>` 格式的稳定 cache key。
//
// convHex = sha256(conversation)[:16] 的 hex（同会话稳定、不同会话不同）。
// 不含账号段：池内轮询/跨池往返时同会话 key 恒定（见包注）。
func BuildCacheKey(conversation string) string {
	sum := sha256.Sum256([]byte(conversation))
	return "wb2a-" + hex.EncodeToString(sum[:16])
}

// strField 从 map 取 string 字段，非 string 或空串返回 ""。
func strField(obj map[string]any, key string) string {
	v, ok := obj[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}
