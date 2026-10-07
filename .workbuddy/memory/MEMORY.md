# workbuddy2api-panel 项目长期记忆

## 项目概况
- Go 语言 AI 网关 + 管理面板：OpenAI 兼容 API（/v1/chat/completions、/v1/models）+ 内嵌 Web 面板（/panel/）。
- 入口 `cmd/server`，核心包：internal/server（HTTP）、internal/pool（账号池选号）、internal/upstream（上游）、internal/scheduler（定时任务）、internal/panel（面板）、internal/modelgroup（模型组，2026-10-06 新增）。
- 监听 :7863，config.json（由 config.example.json 复制，api_key=test_key），凭证落盘 auths/，状态落盘 data/。

## 开发环境约定
- Go 1.27.1 安装在 d:\devhome\Go\，编译前 `set PATH=D:\devhome\Go\bin;%PATH%`。
- GOPROXY 已 `go env -w GOPROXY=https://goproxy.cn,direct`（默认 proxy.golang.org 会 Bad Gateway）。
- 编译运行：`go build -o wb2api.exe ./cmd/server && wb2api.exe -config config.json`。

## 关键实现决策
- 模型组路由：不能用 ServeMux 通配段（/{g}/v1 与 /v1/{g} 两形态互不为子集，注册期 panic），用 "/" 兜底分发器 groupDispatch 手动解析 + r.SetPathValue。主形态 /{组名}/v1（符合 OpenAI SDK base_url 以 /v1 结尾约定），/v1/{组名} 为别名。默认 /v1 完全向后兼容。
- 模型组语义：models 空=不限；非空=白名单。组内**使用顺序**（默认模型选取 + 组 /models 列表）自 2026-10-07 起按**当前生效积分倍率升序**（限时免费=0 排最前），倍率相同则按配置书写顺序；排序实现见 internal/server/handler.go `groupModelPriority`。accounts 空=跟随池规则（成本分层/粘性）；非空=按序钉死，全不可用 503 不回落。
- 面板配置热生效走 saveConfig 深合并 + 各组件 Set/Replace；模型组为 Registry.Replace。
- rewriteModel 对 body 缺 model 字段时不注入，需要强制写入时用 setModelField。

## 测试与环境坑
- handler 测试：TestModelsGrouped 会污染全局 dynamicModelsCache 负缓存，需 resetModelsCache。
- GET 请求 req.Body 可能为 nil，测试构造时需注意。
- 本机沙箱环境：internal/scheduler 与 internal/upstream 部分绑真实 listener 的测试会挂起（TestRunActivityNowReportsEachAccount 等），与代码改动无关，跑测试时排除这两个包。
- 面板测试文件输出写到项目目录内（/tmp 有时不通）。

## 运行状态（2026-10-07）
- 服务后台运行中（wb2api.exe，:7863，`-config config.json`）；已按当日改动重编译并重启。
- 已创建 dev / chat 两个模型组，端到端验证通过（含组内模型按生效倍率升序、限时免费优先）。
- 面板「模型与档位」中模型组卡片已置顶；编辑表单滚动定位已修（block:'center'）。
- 待用户确认：需求中「工作区」的确切所指（面板无此元素，当前按"视图最上方"处理）。
