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
- 模型组语义：models 空=不限；非空=白名单。组内**使用顺序**（默认模型选取 + 组 /models 列表）自 2026-10-07 起按**当前生效积分倍率升序**（限时免费=0 排最前），倍率相同则按配置书写顺序；排序实现见 internal/server/handler.go `groupModelPriority`。**model 字段缺省、等于组名、或字面量 `default`** 三者等价，均取组内默认模型并改写请求体（主路由不校验模型名，原样透传上游）。accounts 空=跟随池规则（成本分层/粘性）；非空=按序钉死，全不可用 503 不回落。
- 组名保留字：internal/modelgroup/modelgroup.go 的 `reserved` = v1/panel/status/healthz/static/assets/favicon.ico/**chat**。`chat` 单列是因为 server 兜底分发器把它当非模型组请求直接 404，禁掉以对齐两处口径（2026-10-07）。
- 面板配置热生效走 saveConfig 深合并 + 各组件 Set/Replace；模型组为 Registry.Replace。
- rewriteModel 对 body 缺 model 字段时不注入，需要强制写入时用 setModelField。

## 测试与环境坑
- handler 测试：TestModelsGrouped 会污染全局 dynamicModelsCache 负缓存，需 resetModelsCache。
- GET 请求 req.Body 可能为 nil，测试构造时需注意。
- 本机沙箱环境：internal/scheduler 与 internal/upstream 部分绑真实 listener 的测试会挂起（TestRunActivityNowReportsEachAccount 等），与代码改动无关，跑测试时排除这两个包。
- 面板测试文件输出写到项目目录内（/tmp 有时不通）。
- 网关 API 路由**强制鉴权**：`Authorization: Bearer <config.api_key>`（当前 api_key=test_key），`httpauth.VerifyBearer` 只认这一种形态且前缀大小写敏感；缺失 → 401。
- 网关**无 CORS**：OPTIONS 预检 404、无 `Access-Control-Allow-Origin` → 浏览器/Electron 网页内核客户端会被拦（CLI/SDK 不受影响）。
- 本机 **IPv6 回环不可用**（`ping ::1` = General failure），`http://[::1]:7863` 连不上；本地测试用 `127.0.0.1` 或 `localhost`。
- 网关无任何代理环境变量；`localhost` → `127.0.0.1`。
- 客户端常见"connection error"排查顺序：① base 与工具的 /v1 拼接规则是否匹配（`/chat2/v1/v1/...`、尾斜杠、`/messages` 均 404，网关只实现 OpenAI 形态）② API Key 是否填对 ③ 是否浏览器内核（CORS）④ 是否 IPv6 回环。
- cmd.exe 不认单引号：`curl -d '{"k":"v"}'` 会把 `'` 原样发出 → 上游 11101 `invalid character '\''`（网关原样透传上游错误）。cmd 用外层双引号 + 内层 `\"`，或 `--data-binary @file`（UTF-8 无 BOM，避开 GBK 乱码）。

## 运行状态（2026-10-07）
- 服务后台运行中（wb2api.exe，:7863，`-config config.json`，PID 55256，12:52 启动）；已按当日改动重编译并重启。
- 模型组：**dev** 与 **chat2**（原名 `chat`，因 `chat` 被列为保留字而改名，模型清单保留）两个组。
- 端到端验证通过：组内模型按生效倍率升序、限时免费优先；组路由 `model="default"` 等同缺省（→ 200 且自动选组内最低生效倍率模型）；`/chat/v1/...` → 404。
- 面板「模型与档位」中模型组卡片已置顶；编辑表单滚动定位已修（block:'center'）。
- 遗留：用户实际报 "connection error" 的客户端/工具名与完整报错原文未知，尚未定位到该客户端侧根因；「工作区」的确切所指亦未确认（面板无此元素）。
