// modelgroups.go 模型组的面板管理接口：
//
//	GET  /panel/api/model_groups  当前组清单 + 池内账号摘要（前端编辑器数据源）
//	POST /panel/api/model_groups  整体替换组清单（校验 → 经 SaveConfig 深合并落盘
//	                              → 注册表热替换，路由立即生效、无需重启）
//
// POST 只提交 model_groups 一个键，main.saveConfig 的深合并逻辑会保留 config.json
// 其余字段（含用户手写的未知键），与配置页的落盘口径一致。
package panel

import (
	"encoding/json"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/modelgroup"
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
	})
}
