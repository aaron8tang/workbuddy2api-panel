// modelgroups.go 模型组的面板管理接口：
//
//	GET  /panel/api/model_groups  当前组清单 + 池内账号摘要（前端编辑器数据源）
//	POST /panel/api/model_groups  整体替换组清单（校验 → 思考档位自动更正 →
//	                              经 SaveConfig 深合并落盘 → 注册表热替换，
//	                              路由立即生效、无需重启）
//
// POST 只提交 model_groups 一个键，main.saveConfig 的深合并逻辑会保留 config.json
// 其余字段（含用户手写的未知键），与配置页的落盘口径一致。
package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/modelgroup"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// modelGroupsGet 返回当前组清单与账号摘要。
// 账号摘要只取选号相关的展示字段（uid/昵称/域/禁用态），不含任何凭证。
func (p *Panel) modelGroupsGet(w http.ResponseWriter, r *http.Request) {
	groups := p.cfg.ModelGroups.List()
	if groups == nil {
		groups = []modelgroup.Group{}
	}
	type acctRow struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
		Realm    string `json:"realm"`
		Disabled bool   `json:"disabled"`
	}
	accounts := make([]acctRow, 0)
	for _, s := range p.cfg.Pool.List() {
		accounts = append(accounts, acctRow{
			UID:      s.UID,
			Nickname: s.Nickname,
			Realm:    s.Realm,
			Disabled: s.Disabled,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"groups":   groups,
		"accounts": accounts,
	})
}

// modelGroupsSave 整体替换模型组清单。
// 请求体 {"groups":[{name,models,accounts}]}；groups 可为空数组（清空全部组）。
// 校验失败 400；SaveConfig 未装配（裸面板测试场景）501。
func (p *Panel) modelGroupsSave(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Groups []modelgroup.Group `json:"groups"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeErr(w, http.StatusBadRequest, "parse body: "+err.Error())
		return
	}
	if payload.Groups == nil {
		payload.Groups = []modelgroup.Group{}
	}
	// 校验 + 就地规范化（trim 组名/模型/账号条目）。失败整体拒绝，不落半份。
	if err := modelgroup.Validate(payload.Groups); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// 思考档位自动更正：条目形如 "cn:glm-5.2|max" 但模型实际不支持 max 时，
	// 按强弱序校正到该模型支持的最近档（|max→最高档、|low→最低档），
	// 未知能力（目录未刷新/渠道不可服务/档位词不在强弱序）的条目原样保留，不臆造。
	fixes := p.fitGroupEfforts(payload.Groups)
	if p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config saving not available")
		return
	}
	// 只带 model_groups 一个键走既有保存管线：深合并保留其余配置，ParseConfig
	// 同套校验兜底，原子落盘，SaveConfig 内部完成注册表热替换（main 闭包）。
	raw, err := json.Marshal(map[string]any{
		"model_groups": map[string]any{"groups": payload.Groups},
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "marshal config: "+err.Error())
		return
	}
	if _, err := p.cfg.SaveConfig(raw); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	groups := p.cfg.ModelGroups.List()
	if groups == nil {
		groups = []modelgroup.Group{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"groups": groups,
		// effort_fixes 本次保存被自动更正的条目（前端提示条用）。
		// 形如 [{"group":"dev","from":"cn:glm-5.2|max","to":"cn:glm-5.2|high","reason":"..."}]
		"effort_fixes": fixesOrEmpty(fixes),
	})
}

// fixesOrEmpty 空更正集返回空数组而非 JSON null：前端直接读 effort_fixes.length
// 判断是否弹提示条，null.length 抛错会把「保存成功」误报成「保存失败」。
func fixesOrEmpty(fixes []effortFix) []effortFix {
	if fixes == nil {
		return []effortFix{}
	}
	return fixes
}

// effortFix 一次档位更正记录（回显给用户，说明"为什么变了"）。
type effortFix struct {
	Group  string `json:"group"`
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// fitGroupEfforts 就地校正组内所有带 "|档位" 后缀的条目到模型实际支持的档位，
// 返回更正记录。模型能力未知（目录未刷新、渠道未装配/不可服务）时跳过该条目——
// 宁可保留用户的配置，也不凭猜测改写。
func (p *Panel) fitGroupEfforts(groups []modelgroup.Group) []effortFix {
	var fixes []effortFix
	for gi := range groups {
		g := &groups[gi]
		for mi, entry := range g.Models {
			model, effort := modelgroup.SplitEffort(entry)
			if effort == "" {
				continue // 未加注档位 → 不变
			}
			supported := p.supportedEffortsFor(model)
			if len(supported) == 0 {
				continue // 能力未知 → 不动
			}
			fitted, changed := modelgroup.FitEffort(effort, supported)
			if !changed {
				continue // 该档位模型支持 → 不变
			}
			fixed := model + "|" + fitted
			g.Models[mi] = fixed
			fixes = append(fixes, effortFix{
				Group:  g.Name,
				From:   entry,
				To:     fixed,
				Reason: fmt.Sprintf("模型不支持 %s，已更正为 %s（支持档：%s）", effort, fitted, strings.Join(supported, "/")),
			})
		}
	}
	return fixes
}

// supportedEffortsFor 查单个模型条目（可带 realm 前缀）的思考档位支持表。
// 空 = 能力不可知（调用方据此跳过更正、保留用户原配置）。
//
// 数据源与 /panel/api/models 的 EffortListing 同口径（远端权威 ∪ 静态兜底）：
//   - cn/global：Upstream effort 探测快照，remote 权威优先、缺失落产品静态表
//   - trae/qoder/qoderwork/qwenwork：**产品静态表只覆盖 cn/global 两域**，
//     这三族没有自己的档位表，且不能拿 CN 表按裸名套（trae:glm-5.2 与
//     cn:glm-5.2 是不同上游、能力未必相同）→ 返回空 = 能力不可知，
//     自动更正跳过（保留用户原配置，不凭猜测改写）。
func (p *Panel) supportedEffortsFor(entry string) []string {
	realm, bare := splitRealm(entry)
	if p.cfg.Upstream == nil {
		return nil
	}
	switch realm {
	case "cn", "global":
		remote, remoteDefault := p.cfg.Upstream.EffortSnapshot(realm)
		efforts, _ := upstream.EffortListing(realm, bare, remote[bare], remoteDefault[bare])
		return efforts
	}
	return nil
}

// splitRealm 剥条目前缀取 (realm, 裸名)；无前缀按 cn（与 server.resolveModel 同口径）。
func splitRealm(entry string) (realm, bare string) {
	idx := strings.IndexByte(entry, ':')
	if idx < 0 {
		return "cn", entry
	}
	switch prefix := entry[:idx]; prefix {
	case "cn", "global", "trae", "qoder", "qoderwork", "qwenwork":
		return prefix, entry[idx+1:]
	}
	return "cn", entry
}
