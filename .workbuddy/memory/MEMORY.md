# workbuddy2api-panel 项目长期记忆

## 项目概况
- Go 语言 AI 网关 + 管理面板：OpenAI 兼容 API（/v1/chat/completions、/v1/models）+ 内嵌 Web 面板（/panel/）。
- 入口 `cmd/server`，核心包：internal/server（HTTP）、internal/pool（账号池选号）、internal/upstream（上游）、internal/scheduler（定时任务）、internal/panel（面板）、internal/modelgroup（模型组，2026-10-06 新增）、**internal/trae（TRAE SOLO 渠道子系统，2026-10-11 阶段 2b 自 trae2api 彻底合并：根包 gateway/engine/chat/callback/login + auth/ pool/ upstream/ scheduler/ 四子包）**。
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
- **三个挂载点**（`docker inspect` 实测）：`E:\...\config.json → /app/config.json`、`E:\...\data → /app/data`、`E:\...\auths → /app/auths`。含义：**往 auths/ 放文件等同于写宿主机项目目录**，`docker cp` 可用但直接拷宿主机路径效果等价且更省事。
- `docker compose up -d --build` 仍不可用（build: . 会重新解析 FROM，理论可走 retag 镜像，但 compose 与 docker run 容器同名需先删；且 compose 的 user env 语义与现容器不同，保持 docker run 方式）。
- **挂载目录不支持 chown/chmod**（Windows bind mount，报 `Operation not permitted`，目录显示 `drwxrwxrwx` 即证据）。`docker cp` 落地的新文件是 `root:root 755`，虽与既有 `app:app 600` 不一致但**可读、不影响服务**，不必修。
- **Git Bash 会把 `docker exec` 的容器内绝对路径当成本地路径转换掉**（报 `C:/Users/.../PortableGit/app/auths: No such file`）。解法：`MSYS_NO_PATHCONV=1 docker exec ...` 或路径写双斜杠 `//app/auths`。`docker cp` 的**源**路径不受影响，只有传给容器的**目标**路径会被转。
- 注意：容器与本地 wb2api.exe 都占用 7863，二者只能起一个。
- **凭证只在启动时加载一次**：`cmd/server/main.go:76` 调 `auth.LoadDir(cfg.AuthDir)` 读 `./auths`，全仓无 fsnotify / 定时重扫。**任何手工往 auths/ 增删凭证文件后必须 `docker restart workbuddy2api` 才生效**。例外：面板内点"添加账号"走 `internal/panel/login.go` 的「落盘 → `pool.Add` 热加载」，那条路径不需重启 —— 两条入口别混。

## 关键实现决策
- 模型组路由：不能用 ServeMux 通配段（/{g}/v1 与 /v1/{g} 两形态互不为子集，注册期 panic），用 "/" 兜底分发器 groupDispatch 手动解析 + r.SetPathValue。默认 /v1 完全向后兼容。
- 模型组语义：models 空=不限；非空=白名单。组内**使用顺序**（默认模型选取 + 组 /models 列表）自 2026-10-07 起按**当前生效积分倍率升序**（限时免费=0 排最前），倍率相同则按配置书写顺序；排序实现见 internal/server/handler.go `groupModelPriority`。**model 字段缺省、等于组名、或字面量 `default`** 三者等价，均取组内默认模型并改写请求体（主路由不校验模型名，原样透传上游）。accounts 空=跟随池规则（成本分层/粘性）；非空=按序钉死，全不可用 503 不回落。
- 组名保留字：internal/modelgroup/modelgroup.go 的 `reserved` = v1/panel/status/healthz/static/assets/favicon.ico/**chat**。`chat` 单列是因为 server 兜底分发器把它当非模型组请求直接 404，禁掉以对齐两处口径（2026-10-07）。
- 面板配置热生效走 saveConfig 深合并 + 各组件 Set/Replace；模型组为 Registry.Replace。
- rewriteModel 对 body 缺 model 字段时不注入，需要强制写入时用 setModelField。
- **trae 渠道分发协议（2026-10-11 阶段 2b）**：resolveModel 第三 realm `"trae"`（`trae:` 前缀模型）；chatCompletions 在 st.done 后、sticky 前整体移交 `trae.Gateway.ChatCompletions`，分发前回填 r.Body（body 已被组裁决改写）+ `st.logged=true` 抑制外层表格行（trae 有自己的 `| trae # |` 行）。Trae==nil（trae.enabled=false 或未装配）→ trae: 前缀恒 404、models 不追加、/panel/api/trae/* 不注册、healthz trae=false，主链路零回归。
- trae 渠道关键配置：auth 共享目录（workbuddy glob `workbuddy*.json`、trae glob `trae-*.json`，trae.auth_dir 空=跟随顶层）；旧独立部署一次性迁移 migrateTraeLegacy（探测 `../trae2api`、`./trae2api`，复制 state+auths，幂等）；16 个 `trae.*` 字段全在 restartRequiredFields（装配期对象不可热替换）；model=default 语义对 trae 条目同样适用（Trae==nil 时跳过该条目）。
- trae 登录回调监听绑 `:18080` 全网卡（非 127.0.0.1）——容器内 Web 登录依赖 docker-proxy 映射，绑 loopback 会静默失效；compose 已加 "18080:18080"（不做 Web 登录可删）。

## 选号与路由机制（排障必读）
所有选号逻辑集中在 `internal/pool/pick.go` 的 `pick(tried, reqModel, realm)`，**两级路径，优先级不同**：

**① 粘性路径（会话级，优先级最高）** — `handler.go:968`
```
:790 sessKey := session.ExtractKey(body)      // 从 body 提取会话键
:795 Session.ResolveForModel(sessKey, model)  // 查已绑定号 → stickyUID
:968 if stickyUID != "" → PickByUIDForModel(stickyUID, model)   // 直接按 UID 取号
:977 if acct == nil → 才回落加权轮换
```
**只要会话有绑定，第一个轮转回合直接按 UID 取号，`pick()` 的全部加权逻辑一律不执行。** 粘性来源优先级（`internal/session/session.go:335` ExtractKey）：`metadata.conversation_id` → `metadata.conversationId` → `conversation_id` → `conversationId` → `prompt_cache_key` → 内容派生键（SHA-256(system+首条user)，前缀 `d-`）；带 `user_id`（metadata 或顶层）时不派生。解绑条件：该号对当前模型被 6004/11102 限额、在途占满、realm 不符、组钉账号但不在清单内（`:801`），否则等 TTL 到期。

**② 加权轮换路径** `pick()` 依次做：
- 候选过滤：healthy/healthyForModel（6004 模型级豁免）→ realm 谓词 → 积分保底 `floorBlocked`（`credit_floor=100`，触底号不接收费模型，tier 0/1 与免费模型豁免）→ 在途占满（cn 3 / global 2）
- **成本分层硬过滤**：tier 0=实测免费 / tier 1=无观测 / tier 2=实测收费，只保留最优层。「无观测」优先于「实测收费」是刻意的：新号限免只能靠实测发现，否则已知收费的号恒压过未知号、免费号永远学不到。条件探索：`cost_explore_interval=30m`，tier 0 垄断且存在 tier 1 时切 tier 1-only 搭车探测一次
- 权重：`credits/maxCredits×10 + 闲置补偿(hours×0.5，上限5.0)`，**有效快过期账号（`expiring_soon=168h` 内）权重 ×3**（`expiringVirtualSlots`，`prefer_expiring=true`）
- top5 截断 → 等权重 Fisher-Yates 洗牌（防字典序饿死）→ `minPickGap=100ms` 防撞号 → 全被用过则 LRU 兜底（按 `usedSeq` 单调序号，非墙钟，避开 Windows ~0.5ms 时钟精度问题）
- 无 healthy 候选 → `pickEarliestExpiryLocked` 兜底（选冷却中最早到期者，禁用/暂停/hard 冷却/保底拦截者均排除）

**排障口诀：请求扎堆某个号，先查粘性，再谈权重。** 典型误判：某号 `credits_expiring=0`（积分到期日超出 168h 窗口）本不该拿大头，若它拿了 90%+ 流量，几乎一定是粘性把加权绕过去了 —— 因为其它号同时具备「闲置补偿拉满 + ×3 快过期加成」，加权路径下反而不会选它。

**粘性是有意的设计代价**：换取上游 prefix cache 命中率（同会话固定落一个号）。失效征兆为日志 `cache miss: model=xx 本次大前缀未命中（prompt=N miss=N，此前观测到过命中）；上游前缀缓存重算，费用会显著升高`。缓解手段副作用递增：等 TTL → 关 `session_sticky.enabled`（长对话费用升）→ 模型组钉死 accounts → 拆多组/多客户端分散会话。

## 测试与环境坑
- handler 测试：TestModelsGrouped 会污染全局 dynamicModelsCache 负缓存，需 resetModelsCache。
- GET 请求 req.Body 可能为 nil，测试构造时需注意。
- 本机沙箱：internal/scheduler 与 internal/upstream 部分绑真实 listener 的测试会挂起（TestRunActivityNowReportsEachAccount 等），跑测试只跑 internal/server 与 internal/modelgroup。**internal/trae 各包测试不挂**（已统一改 RoundTripper stub 注入，不绑端口不触网），全量回归可跑：`go test $(go list ./... | Where-Object { $_ -notmatch 'internal/(scheduler|upstream)$' })`（PowerShell）。
- **PowerShell 5.1 `Set-Content -Encoding utf8` 写 BOM → Go json.Unmarshal `invalid character '\ufeff'` fatal**；写 UTF-8 用 `[System.IO.File]::WriteAllText(path, raw, (New-Object System.Text.UTF8Encoding($false)))`。Write 工具产物也可能带 BOM，喂给程序的文件先查前 3 字节。
- 改 normalize() 链式校验返回链时不得把 `return a()` 直接替换成 `return b()`（曾丢失 normalizePrompt 校验，被 config_test 拦下）。
- 面板测试文件输出写到项目目录内（/tmp 有时不通）。
- PowerShell 内联 JSON 会被吞双引号：写文件后 `-d "@body.json"`，或 cmd.exe 中转义 `\"`。
- API Key 为 test_key（下划线），写 test-key 报 invalid_api_key。
- 网关 API 路由**强制鉴权**：`Authorization: Bearer <config.api_key>`（当前 api_key=test_key），`httpauth.VerifyBearer` 只认这一种形态且前缀大小写敏感；缺失 → 401。
- 网关**无 CORS**：OPTIONS 预检 404、无 `Access-Control-Allow-Origin` → 浏览器/Electron 网页内核客户端会被拦（CLI/SDK 不受影响）。
- 本机 **IPv6 回环不可用**（`ping ::1` = General failure），`http://[::1]:7863` 连不上；本地测试用 `127.0.0.1` 或 `localhost`。
- 网关无任何代理环境变量；`localhost` → `127.0.0.1`。
- 客户端常见"connection error"排查顺序：① base 与工具的 /v1 拼接规则是否匹配（`/chat2/v1/v1/...`、尾斜杠、`/messages` 均 404，网关只实现 OpenAI 形态）② API Key 是否填对 ③ 是否浏览器内核（CORS）④ 是否 IPv6 回环。
- cmd.exe 不认单引号：`curl -d '{"k":"v"}'` 会把 `'` 原样发出 → 上游 11101 `invalid character '\''`（网关原样透传上游错误）。cmd 用外层双引号 + 内层 `\"`，或 `--data-binary @file`（UTF-8 无 BOM，避开 GBK 乱码）。

## 运行状态（2026-10-11 07:46 实测；18:0x trae 合并进展见下）
- **trae 阶段 2b 合并已完成（2026-10-11 晚）**：internal/trae 子系统 + 分发边界 + 面板 TRAE 渠道视图 + config trae 段 + Docker(18080) + 95 个移植测试全绿 + 冒烟通过（enabled：status 200/chat 503 无账号；disabled：双 404）。**代码未提交、未构建新镜像**；当前容器跑的仍是旧镜像（无 trae 渠道）。D:\projects\trae2api 独立项目保留作参考，其 data/auths 会在新 binary 首启时经 migrateTraeLegacy 自动迁移。
- 服务以 **docker 容器（本地构建镜像 workbuddy2api-panel:local）** 运行中（:7863，healthy）；**账号池 6/6 healthy**：柒佰纳米62d9622b / jiujiu 8400b9b6 / 15711405270 89eb6478 / 陈九川 a9df9de4 / **cookman b020e710（新，49361 积分）** / airwa fcff07d7。cn 域可服务、global 不可服务。**wb2api.exe 未运行**（与容器争 7863，只能起一个）。
- 模型组 dev 已在 config.json 落盘并生效（hy4-preview/space-bunny/glm-5.3-flash/deepseek-v4.1-flash/deepseek-v4-flash，accounts 空跟随池）。
- dev 组实测使用顺序：space-bunny → glm-5.3-flash → deepseek-v4.1-flash → deepseek-v4-flash → hy4-preview（末位=当前动态目录其倍率未知；生效后免费模型自动排最前）。
- **chat2 组（原名 chat，因 chat 为保留字改名）配置未落盘**：config.json 的 model_groups 只有 dev 组；当前容器中 /chat2/v1/* 报 unknown_model_group（实测 404），待用户确认组内容后在面板重建。
- 面板「模型与档位」中模型组卡片已置顶；编辑表单滚动定位已修（block:'center'）。
- 相关配置现状（config.json）：`session_sticky={enabled:true, ttl:30m, gc_interval:5m}`、`pool={max_in_flight:3, max_in_flight_global:2, credit_floor:100, prefer_expiring:true, **expiring_soon:48h（2026-10-11 由 168h 改）**, cost_explore_interval:30m, idle_weight_per_hour:0.5, idle_weight_max:5.0}`。
- **成本观测池级共享已上线（2026-10-11）**：限免/峰谷按模型全局生效，任一号实测结论代表全池（pick.go `sharedModelCost`，costTier/floor 回退共享，账号级优先）。旧「唯一 tier 0 假垄断」已修；实测探测落 airwa 验证通过。
- **模型组思考强度注入已上线（2026-10-11）**：组条目支持 `模型|档位` 语法（如 `cn:glm-5.3-flash|max`），组路由强制注入/覆盖出站 body 的 reasoning_effort（客户端自带值也覆盖）。dev 组 4 模型全部 |max，实测思考量提升 ~20 倍（glm-5.3-flash 409 vs 默认 11-28）。**关键上游行为：glm-5.3-flash 只认 max，传 high/low/medium/xhigh 会把思考关掉**；deepseek 系缺档有回退补 high 机制（组配置优先）。面板组编辑器支持该语法与档位 tag 展示。
- hy4-preview 限免已结束（2026-10-11 实测 credit=1.17/次）。
- 待用户确认：需求中「工作区」的确切所指（面板无此元素，当前按"视图最上方"处理）。

## 验证速查
- `curl.exe -s http://localhost:7863/healthz` → `{"healthy":N,"realm_servable":{"cn":true,"global":false},"total":N}`，**N 即账号池规模，改池后必查**。
- 模型列表带 `Authorization: Bearer test_key`；聊天 POST /v1/chat/completions（body 建议走文件）。模型 ID 支持 cn: 前缀完整名或去前缀短名（如 deepseek-v4-flash）。
- 容器日志：`docker logs workbuddy2api 2>&1 | grep "loaded.*account"` 看加载了几个号；请求流水表（`#001 | 时间 | 模型 | stream | 状态 | 昵称 | TTFB | tok | src=`）由 scheduler 周期打印。
- 排障请求落在哪个号：`data/request-logs/requests-YYYY-MM-DD.jsonl`（字段：time/request_id/path/**account**/model/status/duration_ms/ttfb_ms/attempts/tokens/client_ip/user_agent），配 `python -c` 按 account 聚合计数最快。**注意先看 `path` 字段确认真实走的路由**（`/v1/...` 还是 `/dev/v1/...`）。

## 待办
- **提交 trae 阶段 2b 合并改动并重建容器**（internal/trae 子树 + server/panel/main/config 接入 + Dockerfile/compose + config.example.json + 测试，全部未提交）；更新流程照旧：`update-docker.bat`（构建→删旧容器→起新容器→健康探测，数据挂载天然保留）。重建后 healthz 应含 `"trae":false`（realm 已注册、无账号）。
- **trae 渠道真实账号端到端验证**：面板「TRAE 渠道」Web 登录（容器需 18080 映射）或粘贴回调链接导入 → 查积分/签到 → `trae:glm-5.2` 聊天冒烟；顺带验证 migrateTraeLegacy 对 D:\projects\trae2api 旧数据的迁移。
- **客户端 base_url 实际配的是 `http://127.0.0.1:7863/v1`（主路由），不是 `/dev/v1`** —— request-logs 证实当日 15 条全是 `/v1/chat/completions`。若要真正走模型组路由须改 base_url（两者共用账号池规则，行为一致但路由不同）。待用户确认客户端配置。
- **「按临近度分档加权」待用户决策**：expiring_soon 已收窄为 48h，但窗口内一刀切 ×3（airwa 2h 后到期与陈九川 17.6h 后到期权重仍近乎并列）。要「越临期越优先」需改权重公式（如 <6h ×5 / <24h ×3 / <48h ×2）。
- config.json 的 api_key 由示例值 test_key 换成强密钥（公网必须 HTTPS 反代 + 强密钥，服务无内置 TLS）。
- **重建 chat2 模型组**：配置未落盘（当前 /chat2/v1/* 报 unknown_model_group），组内模型/账号清单需用户确认后经面板重建。
- 提交未提交改动：internal/modelgroup/modelgroup.go、modelgroup_test.go、internal/server/handler.go、handler_modelgroup_test.go（chat 保留字，10-07）+ internal/pool/pick.go、pool_test.go（观测共享，10-11）+ config.json（expiring_soon=48h）+ .gitattributes 未跟踪。
- 遗留：用户实际报 "connection error" 的客户端/工具名与完整报错原文未知，尚未定位到该客户端侧根因；「工作区」的确切所指亦未确认（面板无此元素）。