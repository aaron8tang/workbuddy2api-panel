// trae.go 面板 trae 渠道端点（阶段 2b 合并）：把 Gateway 的账号管理 / Web 登录 /
// 签到 handler 挂到 /panel/api/trae/* 前缀下，鉴权与既有面板端点同口径（withAuth）。
//
// 渠道关闭（main 未装配 Gateway，trae.enabled=false）时不注册任何端点：
// 前端探测 404 即显示「渠道未启用」，主面板零回归。
package panel

import (
	"net/http"
)

// traeRoutes 注册 trae 渠道端点（Trae==nil 时不注册）。
func (p *Panel) traeRoutes() {
	g := p.cfg.Trae
	if g == nil {
		return
	}
	p.mux.HandleFunc("GET /panel/api/trae/status", p.withAuth(p.traeStatus))
	p.mux.HandleFunc("GET /panel/api/trae/accounts", p.withAuth(g.AccountsHandler))
	p.mux.HandleFunc("POST /panel/api/trae/accounts/import", p.withAuth(g.ImportHandler))
	p.mux.HandleFunc("DELETE /panel/api/trae/accounts/{uid}", p.withAuth(g.DeleteHandler))
	p.mux.HandleFunc("PATCH /panel/api/trae/accounts/{uid}", p.withAuth(g.PatchHandler))
	p.mux.HandleFunc("POST /panel/api/trae/accounts/{uid}/refresh", p.withAuth(g.RefreshHandler))
	p.mux.HandleFunc("GET /panel/api/trae/accounts/{uid}/json", p.withAuth(g.JSONHandler))
	p.mux.HandleFunc("POST /panel/api/trae/accounts/{uid}/credits", p.withAuth(g.CreditsHandler))
	p.mux.HandleFunc("POST /panel/api/trae/login", p.withAuth(g.LoginStartHandler))
	p.mux.HandleFunc("GET /panel/api/trae/login/result", p.withAuth(g.LoginResultHandler))
	p.mux.HandleFunc("POST /panel/api/trae/login/cancel", p.withAuth(g.LoginCancelHandler))
	p.mux.HandleFunc("POST /panel/api/trae/checkin", p.withAuth(g.CheckinHandler))
}

// traeStatus 渠道总览（前端区块头渲染用）：enabled（网关已装配）+ servable
//（存在可服务账号）+ accounts（复用 Gateway 脱敏列表，免二次请求）。
func (p *Panel) traeStatus(w http.ResponseWriter, r *http.Request) {
	g := p.cfg.Trae
	if g == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":  true,
		"servable": g.Servable(),
		"accounts": g.Accounts(),
	})
}
