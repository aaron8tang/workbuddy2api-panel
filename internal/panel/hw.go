// hw.go 面板华为云两渠道（codearts / officeace）端点：把网关的账号管理 handler
// 挂到 /panel/api/{codearts,officeace}/* 前缀下，鉴权与既有面板端点同口径（withAuth）。
//
// 渠道关闭（main 未装配网关）时不注册任何端点：前端探测 404 即显示「渠道未启用」，
// 主面板零回归。
package panel

// hwRoutes 注册华为云两渠道端点（各网关 nil 时不注册对应前缀）。
func (p *Panel) hwRoutes() {
	if g := p.cfg.CodeArts; g != nil {
		prefix := "/panel/api/codearts"
		p.mux.HandleFunc("GET "+prefix+"/status", p.withAuth(g.StatusHandler))
		p.mux.HandleFunc("GET "+prefix+"/accounts", p.withAuth(g.AccountsHandler))
		p.mux.HandleFunc("POST "+prefix+"/accounts/import", p.withAuth(g.ImportHandler))
		p.mux.HandleFunc("DELETE "+prefix+"/accounts/{uid}", p.withAuth(g.DeleteHandler))
		p.mux.HandleFunc("PATCH "+prefix+"/accounts/{uid}", p.withAuth(g.PatchHandler))
		p.mux.HandleFunc("POST "+prefix+"/accounts/{uid}/refresh", p.withAuth(g.RefreshHandler))
		p.mux.HandleFunc("GET "+prefix+"/accounts/{uid}/json", p.withAuth(g.JSONHandler))
		p.mux.HandleFunc("POST "+prefix+"/accounts/{uid}/balance", p.withAuth(g.BalanceHandler))
		p.mux.HandleFunc("POST "+prefix+"/login", p.withAuth(g.LoginStartHandler))
		p.mux.HandleFunc("GET "+prefix+"/login/result", p.withAuth(g.LoginResultHandler))
		p.mux.HandleFunc("POST "+prefix+"/login/cancel", p.withAuth(g.LoginCancelHandler))
		p.mux.HandleFunc("POST "+prefix+"/models/refresh", p.withAuth(g.ModelsRefreshHandler))
		// 每日福利动作（福利池档案注册/领取，幂等）。
		p.mux.HandleFunc("POST "+prefix+"/checkin", p.withAuth(g.CheckinHandler))
	}
	if g := p.cfg.OfficeAce; g != nil {
		prefix := "/panel/api/officeace"
		p.mux.HandleFunc("GET "+prefix+"/status", p.withAuth(g.StatusHandler))
		p.mux.HandleFunc("GET "+prefix+"/accounts", p.withAuth(g.AccountsHandler))
		p.mux.HandleFunc("POST "+prefix+"/accounts/import", p.withAuth(g.ImportHandler))
		p.mux.HandleFunc("DELETE "+prefix+"/accounts/{uid}", p.withAuth(g.DeleteHandler))
		p.mux.HandleFunc("PATCH "+prefix+"/accounts/{uid}", p.withAuth(g.PatchHandler))
		p.mux.HandleFunc("POST "+prefix+"/accounts/{uid}/balance", p.withAuth(g.BalanceHandler))
		p.mux.HandleFunc("POST "+prefix+"/checkin", p.withAuth(g.CheckinHandler))
		p.mux.HandleFunc("POST "+prefix+"/login", p.withAuth(g.LoginStartHandler))
		p.mux.HandleFunc("GET "+prefix+"/login/result", p.withAuth(g.LoginResultHandler))
		p.mux.HandleFunc("POST "+prefix+"/login/cancel", p.withAuth(g.LoginCancelHandler))
		p.mux.HandleFunc("POST "+prefix+"/models/refresh", p.withAuth(g.ModelsRefreshHandler))
	}
}
