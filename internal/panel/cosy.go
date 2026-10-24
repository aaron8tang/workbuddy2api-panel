// cosy.go 面板 COSY 协议族三渠道端点：把 qoder / qoderwork / qwenwork 网关的
// 账号管理 handler 挂到 /panel/api/{qoder,qoderwork,qwenwork}/* 前缀下，
// 鉴权与既有面板端点同口径（withAuth）。
//
// 渠道关闭（main 未装配网关）时不注册任何端点：前端探测 404 即显示「渠道未启用」，
// 主面板零回归。
package panel

import (
	"net/http"
)

// cosyRoutes 注册三渠道端点（各网关 nil 时不注册对应前缀）。
func (p *Panel) cosyRoutes() {
	if g := p.cfg.Qoder; g != nil {
		prefix := "/panel/api/qoder"
		p.mux.HandleFunc("GET "+prefix+"/status", p.withAuth(func(w http.ResponseWriter, r *http.Request) {
			p.cosyStatus(w, g.Servable(), g.HasAccounts())
		}))
		p.mux.HandleFunc("GET "+prefix+"/accounts", p.withAuth(g.AccountsHandler))
		p.mux.HandleFunc("POST "+prefix+"/accounts/import", p.withAuth(g.ImportHandler))
		p.mux.HandleFunc("DELETE "+prefix+"/accounts/{uid}", p.withAuth(g.DeleteHandler))
		p.mux.HandleFunc("PATCH "+prefix+"/accounts/{uid}", p.withAuth(g.PatchHandler))
		p.mux.HandleFunc("POST "+prefix+"/accounts/{uid}/refresh", p.withAuth(g.RefreshHandler))
		p.mux.HandleFunc("GET "+prefix+"/accounts/{uid}/json", p.withAuth(g.JSONHandler))
		p.mux.HandleFunc("POST "+prefix+"/login", p.withAuth(g.LoginStartHandler))
		p.mux.HandleFunc("GET "+prefix+"/login/result", p.withAuth(g.LoginResultHandler))
		p.mux.HandleFunc("POST "+prefix+"/login/cancel", p.withAuth(g.LoginCancelHandler))
		p.mux.HandleFunc("POST "+prefix+"/models/refresh", p.withAuth(g.ModelsRefreshHandler))
	}
	if g := p.cfg.QoderWork; g != nil {
		prefix := "/panel/api/qoderwork"
		p.mux.HandleFunc("GET "+prefix+"/status", p.withAuth(func(w http.ResponseWriter, r *http.Request) {
			p.cosyStatus(w, g.Servable(), g.HasAccounts())
		}))
		p.mux.HandleFunc("GET "+prefix+"/accounts", p.withAuth(g.AccountsHandler))
		p.mux.HandleFunc("POST "+prefix+"/accounts/import", p.withAuth(g.ImportHandler))
		p.mux.HandleFunc("DELETE "+prefix+"/accounts/{uid}", p.withAuth(g.DeleteHandler))
		p.mux.HandleFunc("PATCH "+prefix+"/accounts/{uid}", p.withAuth(g.PatchHandler))
		p.mux.HandleFunc("POST "+prefix+"/accounts/{uid}/refresh", p.withAuth(g.RefreshHandler))
		p.mux.HandleFunc("GET "+prefix+"/accounts/{uid}/json", p.withAuth(g.JSONHandler))
		p.mux.HandleFunc("POST "+prefix+"/accounts/{uid}/credits", p.withAuth(g.CreditsHandler))
		p.mux.HandleFunc("POST "+prefix+"/checkin", p.withAuth(g.CheckinHandler))
		p.mux.HandleFunc("POST "+prefix+"/login", p.withAuth(g.LoginStartHandler))
		p.mux.HandleFunc("GET "+prefix+"/login/result", p.withAuth(g.LoginResultHandler))
		p.mux.HandleFunc("POST "+prefix+"/login/cancel", p.withAuth(g.LoginCancelHandler))
		p.mux.HandleFunc("POST "+prefix+"/models/refresh", p.withAuth(g.ModelsRefreshHandler))
	}
	if g := p.cfg.QwenWork; g != nil {
		prefix := "/panel/api/qwenwork"
		p.mux.HandleFunc("GET "+prefix+"/status", p.withAuth(func(w http.ResponseWriter, r *http.Request) {
			p.cosyStatus(w, g.Servable(), g.HasAccounts())
		}))
		p.mux.HandleFunc("GET "+prefix+"/accounts", p.withAuth(g.AccountsHandler))
		p.mux.HandleFunc("POST "+prefix+"/accounts/import", p.withAuth(g.ImportHandler))
		p.mux.HandleFunc("DELETE "+prefix+"/accounts/{uid}", p.withAuth(g.DeleteHandler))
		p.mux.HandleFunc("PATCH "+prefix+"/accounts/{uid}", p.withAuth(g.PatchHandler))
		p.mux.HandleFunc("POST "+prefix+"/accounts/{uid}/refresh", p.withAuth(g.RefreshHandler))
		p.mux.HandleFunc("GET "+prefix+"/accounts/{uid}/json", p.withAuth(g.JSONHandler))
		p.mux.HandleFunc("POST "+prefix+"/login", p.withAuth(g.LoginStartHandler))
		p.mux.HandleFunc("GET "+prefix+"/login/result", p.withAuth(g.LoginResultHandler))
		p.mux.HandleFunc("POST "+prefix+"/login/cancel", p.withAuth(g.LoginCancelHandler))
		p.mux.HandleFunc("POST "+prefix+"/models/refresh", p.withAuth(g.ModelsRefreshHandler))
	}
}

// cosyStatus 渠道总览（前端区块头渲染用）：enabled（网关已装配，本端点注册即真）
// + servable（存在可服务账号）+ has_accounts（池内是否有账号，无论健康与否）。
// 账号清单复用各渠道 accounts 端点（免二次请求逻辑重复）。
func (p *Panel) cosyStatus(w http.ResponseWriter, servable, hasAccounts bool) {
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":      true,
		"servable":     servable,
		"has_accounts": hasAccounts,
	})
}
