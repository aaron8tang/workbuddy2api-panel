// ac.go 面板 AutoClaw 渠道端点（网关 nil 时整组不注册）。
package panel

// acRoutes 注册 AutoClaw 渠道端点（/panel/api/autoclaw/*）。
// 登录是两步：POST login/send-code（发短信）→ POST login/verify（提交验证码）。
func (p *Panel) acRoutes() {
	g := p.cfg.AutoClaw
	if g == nil {
		return
	}
	prefix := "/panel/api/autoclaw"
	p.mux.HandleFunc("GET "+prefix+"/status", p.withAuth(g.StatusHandler))
	p.mux.HandleFunc("GET "+prefix+"/accounts", p.withAuth(g.AccountsHandler))
	p.mux.HandleFunc("POST "+prefix+"/accounts/import", p.withAuth(g.ImportHandler))
	p.mux.HandleFunc("DELETE "+prefix+"/accounts/{uid}", p.withAuth(g.DeleteHandler))
	p.mux.HandleFunc("PATCH "+prefix+"/accounts/{uid}", p.withAuth(g.PatchHandler))
	p.mux.HandleFunc("POST "+prefix+"/accounts/{uid}/balance", p.withAuth(g.BalanceHandler))
	p.mux.HandleFunc("POST "+prefix+"/checkin", p.withAuth(g.CheckinHandler))
	p.mux.HandleFunc("POST "+prefix+"/login/send-code", p.withAuth(g.LoginSendHandler))
	p.mux.HandleFunc("POST "+prefix+"/login/verify", p.withAuth(g.LoginVerifyHandler))
	// 桌面端登录态导入（读本机 %APPDATA%\AutoClaw\auth.json，DPAPI 解密）。
	p.mux.HandleFunc("GET "+prefix+"/desktop", p.withAuth(g.DesktopStatusHandler))
	p.mux.HandleFunc("POST "+prefix+"/desktop/import", p.withAuth(g.DesktopImportHandler))
	p.mux.HandleFunc("POST "+prefix+"/models/refresh", p.withAuth(g.ModelsRefreshHandler))
}
