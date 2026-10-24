# workbuddy2api-panel 项目长期记忆

## 项目概况
- Go 语言 AI 网关 + 管理面板：OpenAI 兼容 API（/v1/chat/completions、/v1/models）+ 内嵌 Web 面板（/panel/）。
- 入口 `cmd/server`，核心包：internal/server（HTTP）、internal/pool（账号池选号）、internal/upstream（上游）、internal/scheduler（定时任务）、internal/panel（面板）、internal/modelgroup（模型组，2026-10-06 新增）、**internal/trae（TRAE SOLO 渠道子系统，2026-10-11 阶段 2b 自 trae2api 彻底合并：根包 gateway/engine/chat/callback/login + auth/ pool/ upstream/ scheduler/ 四子包）**、**internal/hwauth（华为云身份底座：DPoP ES256 + CodeArts SDK / OfficeAce SDK+V11 双签名，2026-10-18，金向量测试钉死）**、**internal/codearts + internal/officeace（华为云两渠道，2026-10-18 自 agent2api 移植）**、internal/qoder（qoder.sh 国际）、internal/qoderwork（qoder.com.cn 中国）、internal/qwenwork（千问办公）、internal/cosy（COSY 协议共用底座）。
- 监听 :7863，config.json（由 config.example.json 复制，api_key=test_key），凭证落盘 auths/，状态落盘 data/（state.json、usage.json、request-logs/ JSONL 归档）。渠道凭证共存：workbuddy*.json / trae-*.json / qoder-*.json / qoderwork-*.json / qwenwork-*.json / **codearts-*.json / officeace-*.json**（glob 前缀互不相交）。
- 版本 appVersion="1.13.0-panel"；路由：/healthz（无可用账号 503，realm_servable 含 trae/qoder/qoderwork/qwenwork/**codearts/officeace**）、/panel/、/status、/v1/*；模型组形态 /{组名}/v1（主）+ /v1/{组名}（别名）；渠道前缀 8 个：trae:/qoder:/qoderwork:/qwenwork:/**codearts:/officeace:**。
- **渠道接线清单（加新渠道照此）**：resolve_model 枚举 → handler.Config 字段+hwChannel/cosyChannel 分发+healthz+models → config.go 渠道段+normalize → main.go 装配+panel/server 传门面+restartRequiredFields → panel.Config 字段+路由文件 → index.html box+app.js（COSY_CHANNELS/登录轮询/渲染）。门面 nil=渠道完全关闭（零回归）。

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
所有选号逻辑分布在**三条路径**（2026-10-11 第十五轮定论，改选号策略必须三路都查）：

**① 粘性快路径（有绑定，优先级最高）** — `handler.go:998`
```
:790 sessKey := session.ExtractKey(body)      // 从 body 提取会话键
:795 Session.ResolveForModel(sessKey, model)  // 查已绑定号 → stickyUID
:998 if stickyUID != "" → PickByUIDForModel(stickyUID, model)   // 直接按 UID 取号
:712 if acct == nil → 才回落加权轮换 pick()
```
**只要会话有绑定，第一个轮转回合直接按 UID 取号，`pick()` 的全部加权逻辑一律不执行。** 粘性来源优先级（`internal/session/session.go:335` ExtractKey）：`metadata.conversation_id` → `metadata.conversationId` → `conversation_id` → `conversationId` → `prompt_cache_key` → 内容派生键（SHA-256(system+首条user)，前缀 `d-`）；带 `user_id`（metadata 或顶层）时不派生。解绑条件：该号对当前模型被 6004/11102 限额、在途占满、realm 不符、组钉账号但不在清单内（`:831`），否则等 TTL 到期。

**①′ 粘性慢路径（新会话分配，选号盲区之首）** — `session.go ResolveForModel` 慢路径
无绑定的新会话在 `ResolveForModel` 内**就地分配**：`allocateSlice(model)` 取分配清单 → 空闲号优先 → 取号。**`pick()` 完全不参与**——带会话键的请求（官方 CLI 等真实客户端全部）新会话选号只由分配清单决定。分配清单 = `pool.AllocationUIDsForModelRealm`（wiring.go `realmAwareAllocateForModel` 注入，2026-10-11 起**含快过期硬优先**：存在有效快过期账号时只分配快过期号，打日志 `[pool] expiring hard tier (sticky alloc) kept=N/M`；**手工使用顺序模式让位**，见 ②）。**取号方式二选一（2026-10-11 起）**：自动模式 `pool2[hashIndex(key,...)]` 稳定哈希；手工顺序模式（`session.Config.AllocOrdered` = `p.AccountOrderActive` 注入）取清单**首个空闲号**（顺序填充，与 pick() 恒第一名语义在粘性约束下等价；无空闲号回落清单首位）。**可用性判定（快路径 availableSet）用未过滤清单**——两者分离（session.Config 的 AvailableForModel vs AllocateForModel），否则既有绑定会被误判不可用而频繁重分配。坑：同 key 同池态下分配结果跨重启一致（hashIndex 确定性，非持久化），别误判成粘性存了盘。

**② 加权轮换路径** `pick()` 依次做：
- 候选过滤：healthy/healthyForModel（6004 模型级豁免）→ realm 谓词 → 积分保底 `floorBlocked`（`credit_floor=100`，触底号不接收费模型，tier 0/1 与免费模型豁免）→ 在途占满（cn 3 / global 2）
- **成本分层硬过滤**：tier 0=实测免费 / tier 1=无观测 / tier 2=实测收费，只保留最优层。「无观测」优先于「实测收费」是刻意的：新号限免只能靠实测发现，否则已知收费的号恒压过未知号、免费号永远学不到。条件探索：`cost_explore_interval=30m`，tier 0 垄断且存在 tier 1 时切 tier 1-only 搭车探测一次
- **快过期硬优先（2026-10-11 由 ×3 软加权升级）**：候选中存在有效快过期账号（`expiringNow`：credits_expiring>0，窗口由 scheduler 计算时施加，当前 24h）时只在快过期账号内选号，日志 `[pool] expiring hard tier kept=N/M`；全不可用自然回退。关闭 `prefer_expiring` 恢复旧语义
- **手工使用顺序（2026-10-11，`pool.account_order`，面板账号池 ↑↓ 维护）**：非空时在最终候选内按顺序取第一个可用者（排序比较器 rank 最高优先 + 截 1）——**压过积分权重与快过期硬优先**，是用户的显式 override；未覆盖账号垫底（垫底层内仍按单价/权重），order 内全不可用回落垫底号，清空恢复加权随机。overview 透出 `account_order`，POST /panel/api/config `{pool:{account_order}}` 热生效。面板账号池展示：order 非空按它显示；否则 FEFO（最早到期升序，需 lastPackages 缓存）；都无则后端原序。**三条路径一致**：pick()（②）与粘性分配（①′，清单按 rank 排序 + AllocOrdered 取首空闲号）都生效；①′ 里硬过滤在手工顺序模式**整体让位**（否则手工拖到第一的远期号被剔除永远轮不到——实测踩过：首版探针落顺序第二名 jiujiu 而非第一名 cookman）
- 权重：`credits/maxCredits×10 + 闲置补偿(hours×0.5，上限5.0)`；×3 虚拟实例权重（`expiringVirtualSlots`）仍在 `WeightedAvailableUIDsForModelRealm`（可用性清单）但分配侧已被硬过滤取代
- top5 截断 → 等权重 Fisher-Yates 洗牌（防字典序饿死）→ `minPickGap=100ms` 防撞号 → 全被用过则 LRU 兜底（按 `usedSeq` 单调序号，非墙钟，避开 Windows ~0.5ms 时钟精度问题）
- 无 healthy 候选 → `pickEarliestExpiryLocked` 兜底（选冷却中最早到期者，禁用/暂停/hard 冷却/保底拦截者均排除）

**排障口诀：请求落错号，先确认走的三条路里哪条（流水行不会告诉你），再谈策略。** 实测教训（素素 14:07 案）：pick() 硬过滤已生效但素素仍被选中 → 在 pick() 无条件插桩日志后连 DEBUG 都不出现 → 请求走的是粘性分配路径。验证选号行为**必须用全新内容探针**（同内容=同派生键=同绑定，跨重启也复现）。

**粘性是有意的设计代价**：换取上游 prefix cache 命中率（同会话固定落一个号）。失效征兆为日志 `cache miss: model=xx 本次大前缀未命中（prompt=N miss=N，此前观测到过命中）；上游前缀缓存重算，费用会显著升高`。缓解手段副作用递增：等 TTL → 关 `session_sticky.enabled`（长对话费用升）→ 模型组钉死 accounts → 拆多组/多客户端分散会话。

## trae 渠道：倍率 / 积分临期 / 4028（2026-10-13 定型）

**倍率数据源（fixB 定论）**：`get_detail_param → config_info_list[].display_contact_config → consumption_rate.data.rate`。实测 glm-5.3-flash=0.06 / glm-5.3=0.78 / kimi-k3=1.83。`fetchDynamicModels` 1h TTL 缓存，`RateOf` 每请求可调安全。**序列化坑（2026-10-13 修）**：RateOf 有值但 `ModelEntries()` 曾漏透 `credits` 字段 → 面板倍率列全显「—」；现动态条目带 `credits`（`fmtCreditsRate` → `x0.06`，与 CN 域 `fmtCreditsPrefix` 同口径），Rate=0 省略不编造，静态回退表不带倍率。

**积分临期闭环（fixCredits 四段）**：① 定点拉取 `CreditsLoop`（启动即刷 + `CreditRefreshEvery` 默认 6h ticker）+「全部签到」共享 `refreshCredits`；② 内存预扣 `DeductCredits(uid, rate, tokens)` 按 rate×**total_tokens** FEFO 扣权益包（ExpireAt 升序，0=无限期排最后）；③ 权威校准池级系数 `calib`（每百万 tokens × 倍率 = 1 消耗的积分数，默认 100.0，EMA(0.5)，clamp [1e-3,1e5]，持久化 state.json `CalibCreditPerMTok`）——obs = (内存余量 − 上游权威余量) / (Σ rate×tokens / 1e6)；**calibrateLocked 跳过条件**：无旧/新快照、期间有包到期清退、delta≤0、样本 <1e5 rateTokens、obs 越界；④ 持久化。**坑：calibrateLocked 必须先读 `sample := e.rateTokens` 后清，否则除零。** 链路活性观测日志 `[trae_pool] deduct <uid>: rate=.. tokens=.. cost=.. credits=..`（flash 0.06 倍率单次仅 0.0006 积分，int64 聚合余额无感，只能靠日志确认）。

**4028 修复（fix4028）**：WorkBuddy CLI 出站带 `destination-addr: 127.0.0.1:7863`，上游 loopback 校验必 4028。upstream payload.go denylist 剥离，日志 `[payload] stripped client-private field "destination-addr" from body`。

**fixA（粘性移除）**：prompt_cache_key 统一后 SOLO/CN 两池及池内账号完全不需要粘性，选号全部走临期优先加权轮换（24h 临期权重）。

## 风控铁律：同渠道多账号绝不并发（2026-10-24 整改定型）
- **2026-10-23 22:00:01 事故**：keepalive 串行但零延迟，5 个 CN 账号 token 刷新落同一秒，上游批量吊销全部会话（12153 invalid_grant）。用户定性：这是机器人指纹，同渠道多账号**绝对不能同时并发操作**。
- **整改（全部批量点已落地）**：新建共享包 `internal/stagger`——`Heavy()`=3s+rand(0~5s)（签到/保活/刷新/任务写操作）、`Light()`=1s+rand(0~2s)（余额/积分包/扫描/昵称/券码），`SetTestFactor(0)` 测试置零（两个 scheduler 包 TestMain）。**first 范式**：`first := true` → 所有 continue 门控后、首个上游调用前 → `if !first { time.Sleep(stagger.X()) }; first = false`。
- 覆盖：主池 scheduler 5 处（checkin/keepalive/balance串行/pkgrefresh串行/streak）、trae scheduler 4 函数、4 渠道 CheckinAll（qoderwork/autoclaw/codearts/officeace）、panel 6 处扇出（syncNicknames/packages/tasksScanAll/startGrowthQueue 扫描/runQueueItems 纯串行/schoolVouchers）。详见 memory/2026-10-24.md。
- **加新批量任务点必须照 first 范式加错峰**；固定间隔也是指纹，必须随机。
- **第二阶段（同日）：批量互斥 + 触发抖动**——① 全局批量闸门 `batchMu` TryLock（两个 scheduler 包 + 4 渠道 `checkinMu` 分层互斥），手动×定时×连点×ticker 全互斥，忙时拒绝（panel balanceAll 忙时 409）；**拆分范式 `RunXxxNow()`=闸门包装+`doXxx()` 本体，持锁内直调 do\* 避免嵌套 TryLock 全跳过**（RunStreakBonusNow 因链式调用无闸门）；② runBatch 并行改串行；③ `stagger.FireJitter()` 0~5min 定时触发抖动（抹「准点整点批量」指纹，仅定时入口、手动不走）。详见 memory/2026-10-24.md 第二阶段。
- **第三阶段（同日）：主池外渠道全量核查**——逐个渠道读源码验证，补两处漏网：① autoclaw `StartHourlyRefresh` 每小时 `force=true` 零延迟刷全部账号（最严重，同 10-23 事故形态）→ 抽 `refreshAllHourly()`，`checkinMu` 互斥 + FireJitter + 账号间 Heavy；② officeace `StartRefresher` 10min 零延迟扫临期号且全仓无调用点（死代码）→ 直接删除（CheckinAll/转发链已有 `ensureControlPlane` 兜底）。**新增后台批量循环必须同时接渠道 checkinMu + FireJitter + Heavy，三者缺一不可。**

## 上下文压缩责任边界（2026-10-13 定论，排障必读）
**cn:step-5-preview 190K tokens 触发压缩是 WorkBuddy 客户端自己的行为，与网关/上游无关。**
- 网关压缩逻辑在 `promptcache`（`CompactBodyWithSummary` 两段式 + `CompactBody` 滑动裁剪），触发条件是**请求体字节 > 2MB**（`MaxCompactBodyBytes = 2 << 20`），**与 token 数完全无关**；190K tokens ≈ 200-400KB 远够不到。入口日志 `compact+summary conv=` / `compact history ... dropped`，搜不到即未触发。
- 网关 `/v1/models` 透出的 `context_length` 是上游权威值（四级查找链第一级），step-5-preview 上游实测 `maxInputTokens=1000000`——客户端看到的是 1M，它按自己的保守阈值（约 63% 有效窗口）在 190K 压缩。
- 上游不通知压缩：请求全 200、无 11115 prompt too long、`ErrPromptTooLong` 分类链路未触发、SSE 无对应事件。
- **直连上游查模型真实上下文的姿势**：读 `/app/auths/workbuddy-*.json` 的 accessToken，GET `https://copilot.tencent.com/console/enterprises/personal/models`，头带 `Authorization: Bearer <tok>` + Origin/Referer `https://copilot.tencent.com`。

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
- **TRAE 渠道排序修复（2026-10-12）**：用户报「全部签到后 chenwei 24h 内无到期积分仍排第一」→ 根因是 chenwei 的 trae `account_order:1` 手工顺序钉死（手工顺序压过积分/到期变化）。修复：① TRAE 头部加「恢复自动排序」按钮（POST `trae/accounts/order` 空数组清空，与主池 btnAutoOrder 对齐）；② TRAE 自动模式展示按 `credit_expires_at` 升序 FEFO（数据后端已透出，纯前端改动）。已 POST 空数组清除线上手工顺序（order_active=false），实测 6 账号 credit_expires_at 全透出。改动仅 internal/panel/index.html + app.js。
- **trae 阶段 2b 合并已完成（2026-10-11 晚）**：internal/trae 子系统 + 分发边界 + 面板 TRAE 渠道视图 + config trae 段 + Docker(18080) + 95 个移植测试全绿 + 冒烟通过（enabled：status 200/chat 503 无账号；disabled：双 404）。**代码未提交、未构建新镜像**；当前容器跑的仍是旧镜像（无 trae 渠道）。D:\projects\trae2api 独立项目保留作参考，其 data/auths 会在新 binary 首启时经 migrateTraeLegacy 自动迁移。
- 服务以 **docker 容器（本地构建镜像 workbuddy2api-panel:local）** 运行中（:7863，healthy）；**账号池 6/6 healthy**：柒佰纳米62d9622b / jiujiu 8400b9b6 / 15711405270 89eb6478 / 陈九川 a9df9de4 / **cookman b020e710（新，49361 积分）** / airwa fcff07d7。cn 域可服务、global 不可服务。**wb2api.exe 未运行**（与容器争 7863，只能起一个）。
- 模型组 dev 已在 config.json 落盘并生效（hy4-preview/space-bunny/glm-5.3-flash/deepseek-v4.1-flash/deepseek-v4-flash，accounts 空跟随池）。
- dev 组实测使用顺序：space-bunny → glm-5.3-flash → deepseek-v4.1-flash → deepseek-v4-flash → hy4-preview（末位=当前动态目录其倍率未知；生效后免费模型自动排最前）。
- **chat2 组（原名 chat，因 chat 为保留字改名）配置未落盘**：config.json 的 model_groups 只有 dev 组；当前容器中 /chat2/v1/* 报 unknown_model_group（实测 404），待用户确认组内容后在面板重建。
- 面板「模型与档位」中模型组卡片已置顶；编辑表单滚动定位已修（block:'center'）。
- 相关配置现状（config.json）：`session_sticky={enabled:true, ttl:30m, gc_interval:5m}`、`pool={max_in_flight:3, max_in_flight_global:2, credit_floor:100, prefer_expiring:true, **expiring_soon:24h（2026-10-11 下午由 48h 改，用户面板操作）**, cost_explore_interval:30m, idle_weight_per_hour:0.5, idle_weight_max:5.0}`；dev 组仅 `cn:glm-5.3|max`（用户 14:44 面板改）。
- **首页「积分到期提醒」统计窗口跟随 expiring_soon（2026-10-11）**：`/panel/api/packages` 响应携带 `expiring_window_hours`（scheduler `ExpiringSoonWindow()`，0=测试面板不下发），前端 `renderExpiry` 按 `winHours/24` 折算天做「N 天内合计」汇总、「N 天/小时内无到期积分」文案与琥珀色点分层；缺字段回落 168h。此前前端写死 7 天，与路由的 24h 窗口口径脱节（用户指出）。改 expiring_soon 后到期卡片口径自动跟随，无需改前端。
- **成本观测池级共享已上线（2026-10-11）**：限免/峰谷按模型全局生效，任一号实测结论代表全池（pick.go `sharedModelCost`，costTier/floor 回退共享，账号级优先）。旧「唯一 tier 0 假垄断」已修；实测探测落 airwa 验证通过。
- **模型组思考强度注入已上线（2026-10-11）**：组条目支持 `模型|档位` 语法（如 `cn:glm-5.3-flash|max`），组路由强制注入/覆盖出站 body 的 reasoning_effort（客户端自带值也覆盖）。dev 组 4 模型全部 |max，实测思考量提升 ~20 倍（glm-5.3-flash 409 vs 默认 11-28）。**关键上游行为：glm-5.3-flash 只认 max，传 high/low/medium/xhigh 会把思考关掉**；deepseek 系缺档有回退补 high 机制（组配置优先）。面板组编辑器支持该语法与档位 tag 展示。
- hy4-preview 限免已结束（2026-10-11 实测 credit=1.17/次）。
- 待用户确认：需求中「工作区」的确切所指（面板无此元素，当前按"视图最上方"处理）。

## 验证速查
- **trae 渠道积分/倍率链路已全量部署验证（2026-10-13）**：面板 `/panel/api/models` 30 个 trae 模型全部带 `credits` 倍率（browser_use_subagent x0.01 / glm-5.3-flash x0.06 / kimi-k3 x1.83 / qwen3.8-max x1.5 等）；`/dev/v1/models` 同步。6 账号全部有 credit_packs（16-22 包/账号）。流式 trae 调用 200，容器日志实证 `[trae_pool] deduct 2556792193683584: rate=0.06 tokens=105 cost=0.0006 credits=4223`。CN 池带 destination-addr 调用成功且日志证实剥离，4028 已修复。
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
- 提交未提交改动（10-13 增量，用户 4 点需求全部实现并已部署验证，**代码全部未 git 提交**）：internal/trae/gateway.go（ModelEntries 透 credits/name + fmtCreditsRate）、internal/trae/chat.go（两处 DeductCredits 挂钩）、internal/trae/logging.go（UsageTokens/usageTotalTokens total 口径）、internal/trae/gateway_rate_test.go（倍率透出 3 测试）、internal/trae/pool/pool.go（calibrateLocked/DeductCredits/load/saveLocked）+ pool_test.go（FEFO/calib/重启持久化 4 测试）、internal/trae/scheduler/scheduler.go（CreditRefreshEvery/RunCreditsNow/CreditsLoop）、internal/trae/upstream/client.go + client_test.go（consumption_rate 解析）、internal/server/handler.go（组裁决 trae 域真倍率）、cmd/server/main.go（CreditsLoop 启动）、internal/upstream/payload.go（destination-addr denylist）。
- 更早未提交清单：internal/modelgroup/modelgroup.go、modelgroup_test.go、internal/server/handler.go、handler_modelgroup_test.go（chat 保留字，10-07）+ internal/pool/pick.go、pool_test.go、realm.go、internal/session/session.go、cmd/server/wiring.go、main.go、cmd/server/config.go（快过期硬优先/粘性分配硬优先/手工使用顺序，10-11）+ internal/panel/panel.go、app.js、index.html、frontend_test.go（到期提醒窗口 + 账号池 FEFO/手工顺序 + 24h/48h 到期列，10-11/12）+ internal/trae/pool/pool.go、pool_test.go、scheduler/scheduler.go、gateway.go、gateway_expiry_test.go、internal/panel/trae.go（trae 24h/48h 到期列 + 手动排序，10-12）+ config.json + .gitattributes 未跟踪。
- 遗留：用户实际报 "connection error" 的客户端/工具名与完整报错原文未知，尚未定位到该客户端侧根因；「工作区」的确切所指亦未确认（面板无此元素）。