# workbuddy2api-panel 项目长期记忆

## 项目概况
- Go 语言 AI 网关 + 管理面板：OpenAI 兼容 API（/v1/chat/completions、/v1/models）+ 内嵌 Web 面板（/panel/）。
- 入口 `cmd/server`，核心包：internal/server（HTTP）、internal/pool（账号池选号）、internal/upstream（上游）、internal/scheduler（定时任务）、internal/panel（面板）、internal/modelgroup（模型组，2026-10-06 新增）。
- 监听 :7863，config.json（由 config.example.json 复制，api_key=test_key），凭证落盘 auths/，状态落盘 data/（state.json、usage.json、request-logs/ JSONL 归档）。
- 版本 appVersion="1.12.0-panel"；路由：/healthz（无可用账号 503）、/panel/、/status、/v1/*；模型组形态 /{组名}/v1（主）+ /v1/{组名}（别名）。

## 开发环境约定
- Go 1.27.1 安装在 d:\devhome\Go\，编译前 `set PATH=D:\devhome\Go\bin;%PATH%`。
- GOPROXY 已 `go env -w GOPROXY=https://goproxy.cn,direct`（默认 proxy.golang.org 会 Bad Gateway）。
- 编译运行：`go build -o wb2api.exe ./cmd/server && wb2api.exe -config config.json`。
- `core.autocrlf=true`：工作区源码为 CRLF，`gofmt -l` 整文件报差异是行尾噪音；新增 LF 文件可 `gofmt -w <file>` 单独格式化。

## 网络与 Docker 环境
- Docker Desktop 29.7.2 / compose v5.5.1 可用；Docker Hub（registry-1.docker.io）DNS 污染被阻断；GHCR、goproxy.cn、docker.m.daocloud.io 可达；dl-cdn.alpinelinux.org（apk 源）可达。
- **从 Hub 拉镜像的替代**：`docker pull docker.m.daocloud.io/library/<name>:<tag>`（不带 docker.io 段）后 `docker tag` 成原名。已 retag 就位：golang:1.23-alpine、alpine:3.20。
- Dockerfile 已本地化适配（2026-10-07）：移除 `# syntax=docker/dockerfile:1`（需 Hub 拉 frontend，经典语法不需要）；build 阶段加 `ENV GOPROXY=https://goproxy.cn,direct`。
- **当前运行方式（2026-10-07 起）**：本地源码构建镜像。**更新用项目根 `./update-docker.sh`（Git Bash/Linux）或 `update-docker.bat`（cmd 双击直跑，纯 ASCII 英文版，任何代码页无乱码）**（构建→删旧容器→起新容器→健康探测，失败自动回滚旧镜像；数据靠挂载天然保留）。手工流程：`docker build -t workbuddy2api-panel:local .` → `docker rm -f workbuddy2api` → run 命令重建。旧 GHCR 镜像留存可回滚。
- `docker compose up -d --build` 仍不可用（build: . 会重新解析 FROM，理论可走 retag 镜像，但 compose 与 docker run 容器同名需先删；且 compose 的 user env 语义与现容器不同，保持 docker run 方式）。
- 注意：容器与本地 wb2api.exe 都占用 7863，二者只能起一个。

## 关键实现决策
- 模型组路由：不能用 ServeMux 通配段（/{g}/v1 与 /v1/{g} 两形态互不为子集，注册期 panic），用 "/" 兜底分发器 groupDispatch 手动解析 + r.SetPathValue。默认 /v1 完全向后兼容。
- 模型组语义：models 空=不限；非空=白名单。组内**使用顺序**（默认模型选取 + 组 /models 列表）自 2026-10-07 起按**当前生效积分倍率升序**（限时免费=0 排最前），倍率相同则按配置书写顺序；排序实现见 internal/server/handler.go `groupModelPriority`。accounts 空=跟随池规则（成本分层/粘性）；非空=按序钉死，全不可用 503 不回落。
- 面板配置热生效走 saveConfig 深合并 + 各组件 Set/Replace；模型组为 Registry.Replace。
- rewriteModel 对 body 缺 model 字段时不注入，需要强制写入时用 setModelField。

## 测试与环境坑
- handler 测试：TestModelsGrouped 会污染全局 dynamicModelsCache 负缓存，需 resetModelsCache。
- GET 请求 req.Body 可能为 nil，测试构造时需注意。
- 本机沙箱：internal/scheduler 与 internal/upstream 部分绑真实 listener 的测试会挂起（TestRunActivityNowReportsEachAccount 等），跑测试只跑 internal/server 与 internal/modelgroup。
- 面板测试文件输出写到项目目录内（/tmp 有时不通）。
- PowerShell 内联 JSON 会被吞双引号：写文件后 `-d "@body.json"`，或 cmd.exe 中转义 `\"`。
- API Key 为 test_key（下划线），写 test-key 报 invalid_api_key。

## 运行状态（2026-10-07）
- 服务以 **docker 容器（本地构建镜像 workbuddy2api-panel:local）** 运行中（:7863，Up healthy）；账号池 3/3 healthy（挂载保留，无需重登），cn 域可服务。
- 模型组 dev 已在 config.json 落盘并生效（hy4-preview/space-bunny/glm-5.3-flash/deepseek-v4.1-flash/deepseek-v4-flash，accounts 空跟随池）；**chat 组配置丢失待用户确认是否重建**。
- dev 组实测使用顺序：space-bunny → glm-5.3-flash → deepseek-v4.1-flash → deepseek-v4-flash → hy4-preview（末位=当前动态目录其倍率未知；生效后免费模型自动排最前）。
- 面板「模型与档位」中模型组卡片已置顶；编辑表单滚动定位已修（block:'center'）。
- 待用户确认：需求中「工作区」的确切所指（面板无此元素，当前按"视图最上方"处理）。

## 验证速查
- `curl.exe -s http://localhost:7863/healthz`；模型列表带 `Authorization: Bearer test_key`；聊天 POST /v1/chat/completions（body 建议走文件）。模型 ID 支持 cn: 前缀完整名或去前缀短名（如 deepseek-v4-flash）。

## 待办
- config.json 的 api_key 由示例值 test_key 换成强密钥（公网必须 HTTPS 反代 + 强密钥，服务无内置 TLS）。
