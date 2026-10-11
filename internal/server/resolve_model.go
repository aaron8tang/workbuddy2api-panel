package server

import "strings"

// resolveModel 解析模型名协议（PLAN D6）：
//
//	分布式前缀： "[realm:]model"
//
// 取第一个 ":"，前段恰为 "cn"/"global"/"trae" 才剥离；否则视为裸名，realm=cn、
// bare=原串。大小写敏感（前缀必须是精确的小写枚举）。bare 即出站/选号/账本使用
// 的裸模型名。
//
// "trae" 前缀（阶段 2b 合并）把请求分发到 TRAE SOLO 渠道：bare 为 trae 渠道内
// 的模型名（glm-5.2 等）。调用方需先按 realm=="trae" 分流，其余 realm 走
// workbuddy 主链路。
func resolveModel(model string) (realm, bare string) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn", model
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" && prefix != "trae" {
		return "cn", model
	}
	return prefix, model[idx+1:]
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }
