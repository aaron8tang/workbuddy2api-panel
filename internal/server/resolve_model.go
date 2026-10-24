package server

import "strings"

// resolveModel 解析模型名协议（PLAN D6）：
//
//	分布式前缀： "[realm:]model"
//
// 取第一个 ":"，前段恰为 "cn"/"global"/"trae"/"qoder"/"qoderwork"/"qwenwork"/
// "codearts"/"officeace" 才剥离；否则视为裸名，realm=cn、bare=原串。
// 大小写敏感（前缀必须是精确的小写枚举）。bare 即出站/选号/账本使用的裸模型名。
//
// "trae" 前缀（阶段 2b 合并）把请求分发到 TRAE SOLO 渠道；"qoder"/"qoderwork"/
// "qwenwork" 前缀分发到 COSY 协议族三渠道（qoder.sh / qoder.com.cn / qwenwork.cn）；
// "codearts"/"officeace" 前缀分发到华为云两渠道（snap-access / AgentArts 网关），
// bare 为各渠道内的模型名。调用方需按 realm 分流，其余 realm 走 workbuddy 主链路。
func resolveModel(model string) (realm, bare string) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn", model
	}
	prefix := model[:idx]
	switch prefix {
	case "cn", "global", "trae", "qoder", "qoderwork", "qwenwork", "codearts", "officeace", "autoclaw":
		return prefix, model[idx+1:]
	}
	return "cn", model
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }
