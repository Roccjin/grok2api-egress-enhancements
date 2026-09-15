# Grok CPA 插件改进方案：可靠升级、凭证初始化与千节点管理

> 状态：设计方案，尚未实施。本文只新增文档，不修改插件、CPA 或 CPAMP 业务代码。
>
> 基于本地代码审阅，不代表已复现用户部署环境中的升级故障；性能数值均为建议验收目标，不是实测结果。

## 1. 结论与推荐顺序

这三个诉求都值得做，但不能仅给插件加一个“启动时初始化”按钮就结束。

1. **先修升级与数据保护**：区分安装成功、注册成功和可用；提供不删除配置的修复入口；解决新旧插件后台任务的生命周期；状态文件异常时不能悄悄改用空配置。
2. **增加“从 CPA Grok 凭证发现节点”**：读取现有 xAI 凭证的 `proxy_url`，去重后建立节点，不重写凭证、不重平衡、不自动启用已停用账号。
3. **增加节点分页**：先解决页面长列表，再让后端真正分页；同时调整概览、跨页选择、轮询和绑定账号弹窗，避免“看起来分页，实际上仍传输所有节点”。
4. **补齐安全操作与规模化运行**：删除/迁移影响预览、逐项失败反馈、限流探测、缓存刷新隔离、可恢复的状态存储。

### 优先级定义

| 优先级 | 目标 | 推荐交付 |
|---|---|---|
| P0 | 不丢配置、不错误覆盖状态、不让多个版本同时写数据 | 生命周期停写、保留配置修复、存储异常保护、明确升级结果 |
| P1 | 直接解决用户日常管理困难 | 凭证发现向导、千节点分页、安全批量操作、绑定账号分页 |
| P2 | 提高规模化运行效率和诊断能力 | 探测调度公平性、增量同步、趋势/审计、Host API 批量摘要 |

**职责边界：**初始化和节点页面主要改插件；“插件未注册时如何修复”“卸载为何删除配置”“升级结果如何展示”必须协同修改 CPA 宿主和 CPAMP，插件自身无法在未被加载时修复自己。

---

## 2. 分析范围、基线与证据索引

### 2.1 本次分析的本地基线

| 项目 | 本地提交/版本 | 角色 |
|---|---|---|
| `grok2api-egress-enhancements` | `b0cb59b`；插件代码版本 `1.1.0` | 原生插件与内嵌页面 |
| `CLIProxyAPI` | `7bbfeaf8` | 安装、加载、注册、凭证 Host API |
| `CPA-Manager-Plus` | `376b1e8c` | 插件商店、插件管理前端 |
| 插件 SDK 依赖 | `CLIProxyAPI/v7 v7.2.113`，`go 1.26.0` | 来自插件 `go.mod`，不等于线上宿主版本 |

“升级”需覆盖三类情形，不能混为一谈：

- **升级插件**：商店下载新二进制，宿主切换运行版本。
- **升级 CPA 或重建容器**：进程重启，重新扫描插件目录、读取配置和状态；依赖挂载、路径、架构与运行环境。
- **仅升级 CPAMP 前端**：主要影响状态展示、请求和缓存；不能仅凭前端升级就断定 `.so` 或状态文件被删除。

### 2.2 关键代码证据

以下行号以本地审阅基线为准，后续实现可能变化；链接指向真实文件。

| 编号 | 代码位置与关键符号 | 已确认事实 |
|---|---|---|
| E01 | [CPA plugins.go](../../CLIProxyAPI/internal/api/handlers/management/plugins.go)，`ListPlugins`，107–145 | `configured` 仅表示配置项存在；`registered` 来自运行时注册列表；生效还要求全局和单插件启用 |
| E02 | [CPA plugin_store.go](../../CLIProxyAPI/internal/api/handlers/management/plugin_store.go)，`InstallPluginFromStore` / `enablePluginConfigLocked`，306–361、490–506 | 安装先保存文件/配置，再异步 reload；正常商店更新保留其他插件 Raw 配置字段；返回 installed 不证明激活成功 |
| E03 | [CPA plugins.go](../../CLIProxyAPI/internal/api/handlers/management/plugins.go)，`DeletePlugin`，322–410 | 删除选中的二进制，并删除整个 `plugins.configs[id]`；该函数没有删除插件自定义状态文件 |
| E04 | [PluginsPage.tsx](../../CPA-Manager-Plus/apps/web/src/features/plugins/PluginsPage.tsx)，396–447；[PluginStorePage.tsx](../../CPA-Manager-Plus/apps/web/src/features/plugins/PluginStorePage.tsx)，801–834 | 两处“重装”都走 DELETE → install，没有回填原插件配置；并不等价于保留配置修复 |
| E05 | [插件 main.go](./go/main.go)，`handleMethod` / `configure`，246–334 | 注册阶段已避免 Host 凭证扫描；但未处理 `plugin.quiesce`；YAML 错误静默回默认；默认路径最后可退到临时目录 |
| E06 | [CPA host.go](../../CLIProxyAPI/internal/pluginhost/host.go)，260–317、341–360、762–767、851–984 | 有 Reconfigure、热替换和运行时回退；quiesce 不支持仍继续替换；retire 只保留旧实例，不停止其后台任务 |
| E07 | [插件 store.go](./go/store.go)，`newStateStore` / `load` / `persistLocked`，298–449 | load 错误被忽略；已有临时文件 rename 和延迟刷盘；rename 前清 dirty，部分刷盘错误被忽略 |
| E08 | [插件 auth_bind.go](./go/auth_bind.go)，`fetchAuthFilesFromHost` / `getAuthFile`，110–232 | 可以 list 后逐个 get 获取 `proxy_url`，但当前没有反向建节点功能；有逐项失败静默跳过 |
| E09 | [插件 auth_bind.go](./go/auth_bind.go)，37–108、322–409 | 已有 60 秒缓存和加载互斥；Fresh 扫描失败也可能返回旧缓存；重平衡会改写凭证代理，不能代替初始化 |
| E10 | [CPA auth_callbacks.go](../../CLIProxyAPI/internal/pluginhost/auth_callbacks.go)，68–113、218–262；[SDK types.go](../../CLIProxyAPI/sdk/pluginapi/types.go)，`HostAuthGetRequest` | 当前宿主 get 只接受 `auth_index`；列表/运行时摘要未提供节点发现所需的代理字段；逐个 get 在宿主内还会遍历账号列表 |
| E11 | [插件 page.html](./go/page.html)，491–578、622–668、1088 | 节点无分页；每 15 秒拉全量状态和节点；桌面表格与移动卡片都全量构建；已有 loading 防重入和页面隐藏暂停 |
| E12 | [accounts-panel.js](./go/accounts-panel.js)，73–98、146–162；[page.html](./go/page.html)，852–868 | 账号降智统计已有前端每页 20 条；“节点绑定账号”弹窗是另一条链路，仍全量展示 |
| E13 | [插件 main.go](./go/main.go)，540–548、573–600、656–670、789–831 | 节点接口全量返回；删除/批量启停忽略部分错误；批量连通测试串行；状态接口附带全量节点及账号统计 |
| E14 | [插件 guard.go](./go/guard.go)，57–125、1390–1438 | 热路径缓存过期仍可能进入 Host 扫描；常规主动探测每 tick 最多一个，隔离复测在同一 worker 串行执行 |
| E15 | [CPA platform.go](../../CLIProxyAPI/internal/pluginhost/platform.go)，119–174、252–288；[config.go](../../CLIProxyAPI/internal/pluginhost/config.go)，109–117 | 配置固定版本缺文件时不会任意选择其他版本；旧文件清理没有“上一已验证版本”保留规则 |
| E16 | [CPA loader_unix.go](../../CLIProxyAPI/internal/pluginhost/loader_unix.go)，111–162、202–223 | 使用 C ABI `dlopen` / `cliproxy_plugin_init`，不是 Go 标准库 `plugin.Open` |
| E17 | [发布工作流](../.github/workflows/cpa-plugin-release.yml)，19–58；[插件 go.mod](./go/go.mod) | 已有 Go 测试、双架构编译和校验和；缺少真实宿主升级/重启/保留配置的集成门禁 |
| E18 | [CPA docker-compose.yml](../../CLIProxyAPI/docker-compose.yml)，24–28；[plugin_path.go](../../CLIProxyAPI/internal/config/plugin_path.go)，14–32 | 示例已有配置、认证、日志、插件二进制挂载；相对插件目录依赖工作目录；示例没有该 Grok 插件专属状态卷 |
| E19 | [CPA config_yaml.go](../../CLIProxyAPI/internal/config/config_yaml.go)，`SaveConfigPreserveComments`，64–82 | 配置保存使用 os.Create 后编码/写入，未采用原子替换；不能把配置保存失败当成无副作用 |
| E20 | [pluginPolling.ts](../../CPA-Manager-Plus/apps/web/src/features/plugins/pluginPolling.ts)，17–25、44–89；[PluginStorePage.tsx](../../CPA-Manager-Plus/apps/web/src/features/plugins/PluginStorePage.tsx)，670–695；[client.ts](../../CPA-Manager-Plus/apps/web/src/services/api/client.ts)，203–224 | 前端已有轮询，但完成条件不要求 registered，成功通知早于轮询；错误对象保留原始 body，但 message 优先取 error 字符串 |

跨仓库相对链接按本工作区三个仓库并列的目录布局编写；单独发布此插件仓库时，应将 CPA/CPAMP 引用改成对应固定提交的仓库链接。

---

## 3. 当前问题怎么理解

### 3.1 “未注册 / 已配置”不是同一件事的矛盾状态

当前 UI 将不同维度并排展示：

```text
配置中存在 grok2api-egress → 已配置
本次运行时注册表没有它   → 未注册
全局开启 + 单插件开启 + 已注册 → 生效
```

因此，“已配置”不意味着二进制存在、配置正确、状态文件读成功，也不意味着节点守护正在工作。

### 3.2 可以确认的缺口与仍需日志验证的原因

| 项目 | 判断 | 与用户症状的关系 |
|---|---|---|
| 卸载重装删除宿主插件配置 | **已确认行为** | 直接解释自定义 `state_file`、轮换配置等重装后需要重填 |
| 正常商店更新主动清空全部插件配置 | **不成立，不能这样归因** | 当前实现保留 Raw 其他字段；需查实际失败阶段 |
| 商店返回 installed 但随后注册失败 | **已确认存在表达缺口** | 用户看到“安装/升级成功”，却没有可用插件 |
| 热切换后旧 worker 没有可靠停写 | **由两侧代码确认的并发风险，未动态复现** | 可造成重复探测、状态写入冲突、旧状态覆盖新状态；不是所有“未注册”的唯一根因 |
| 状态加载失败仍以空默认状态继续 | **已确认风险** | 可能表现为配置/节点“丢了”；后续写入还有覆盖旧文件风险 |
| 重建容器未保留 plugins 或 plugin-data、工作目录改变 | **部署待查** | 能解释二进制消失或读到另一份状态，但本次没有线上挂载证据 |
| 架构、libc、ABI、导出符号不兼容 | **运行环境待查** | 能解释加载失败；需具体 loader 日志与发布包信息 |
| 早期版本注册时同步扫凭证 | **已有历史防护，不应重复当作当前唯一根因** | 当前 `configure` 已不扫凭证，已有对应测试；线上版本仍需核对 |

重要补充：该插件是 **C ABI 原生插件**，不能套用“Go 插件必须与宿主所有 Go 依赖完全一致”的解释。应分别检查 CPU/OS、libc 基线、C ABI 版本、RPC schema 和实际使用的 Host API 能力。

### 3.3 用户所说的“配置”至少有三层

| 层 | 内容 | 保留责任 |
|---|---|---|
| 宿主配置 | `plugins.configs.grok2api-egress`：enabled、state_file、rotation 配置、store manifest 等 | CPA 安装/卸载/修复流程 |
| 插件状态 | 节点、策略、探针方案、隔离状态、统计、事件、节点 ID | 插件状态存储与持久卷 |
| CPA 凭证 | xAI 凭证的 `proxy_url`、disabled 及认证材料 | CPA auth 存储；初始化只读，绑定操作才允许写 |

从凭证恢复节点，只能恢复“代理节点及对应关系”，**不能推导原来的节点名、容量、隔离历史、阈值、探针方案和轮换白名单**。初始化不是完整备份恢复的替代品。

---

## 4. P0：可靠升级和不丢配置

### 4.1 建立用户可理解的运行状态

保留旧 `configured/registered/enabled/effective_enabled` 字段以兼容现有客户端，新增结构化诊断，而不是删掉旧字段：

| 状态 | 展示 | 允许的操作 |
|---|---|---|
| configured_only | 已保存配置，未发现对应二进制 | 检查目录、保留配置安装 |
| disabled | 插件或全局开关关闭 | 启用，不误报故障 |
| loading / registering | 正在加载/注册 | 查看进度，禁止重复提交同一操作 |
| ready | 已注册，配置和状态可用 | 正常管理 |
| degraded | 管理入口可用，但存储或后台守护异常 | 诊断/恢复，禁止有风险的自动修改 |
| failed | 加载/注册失败 | 显示阶段和错误，重试/修复/回退 |
| restart_required | 不能安全在线切换 | 保留配置和待升级版本，安排受控重启 |
| rolled_back | 新版本未生效，已恢复旧版本 | 显示失败的新版本与当前活动版本 |

拟新增诊断字段：

```json
{
  "runtime_status": "failed",
  "desired_version": "next-version",
  "installed_version": "next-version",
  "active_version": "previous-version",
  "last_known_good_version": "previous-version",
  "phase": "register",
  "last_error": {
    "code": "plugin_register_failed",
    "message": "注册失败的脱敏说明",
    "occurred_at": "RFC3339 timestamp",
    "retryable": true
  },
  "operation_id": "opaque-operation-id",
  "restart_required": false
}
```

- `active_version` 由真实运行时提供，不从已下载文件推断。
- `ready` 不要求启动时对所有代理访问外网。加载状态成功、生命周期可控、管理资源可用即可；外网节点质量单独显示。
- 插件未注册时诊断入口仍由宿主提供，不能依赖插件 `/status`。
- 区分 `persisted_at`、`worker_heartbeat_at`、`auth_snapshot_at`，不能用状态文件写入时间假装 worker 心跳。
- 复用 CPAMP 现有轮询，改为消费 `timedOut` 和真实激活状态；超时显示“尚未确认生效”，保留继续查询入口。不能收到安装响应就先弹“升级成功”。
- 错误显示保留稳定 code，同时优先展示经过脱敏的人类可读 message；不要只显示 `plugin_install_failed` 这样的代码。第三方插件修复/重装确认也要明确保留或删除哪些数据。

### 4.2 “修复”与“卸载”分开

**新增用户主入口：`修复安装（保留配置与节点）`。**

修复顺序：

1. 读取宿主配置、目标版本、来源及已安装文件信息，检查路径与权限。
2. 原文件有效且仅是可重试注册失败：安全重试加载/注册。
3. 文件缺失或损坏：按已固定来源和版本重新下载、校验，保留 Raw 配置。
4. 需要切换加载对象时走安全生命周期；同路径但内容变了，不能仅调用 Reconfigure 就称为修复成功。
5. 激活失败保留错误、配置、旧版本；不自动转成 DELETE → install。

**兼容方式：**

- 保留现有安装 API，增加操作状态和激活结果字段。
- 新增宿主 `POST /v0/management/plugins/:id/repair`，明确 `mode: retry | reinstall`、固定 source/version；这是拟议接口，当前不存在。
- 现有 DELETE 若增加 `preserve_config=true`，旧客户端未传参时仍维持旧协议；新版 CPAMP 默认勾选“保留配置与插件数据”。
- “彻底移除配置”是单独危险选项，需要二次确认，不能默认用于排障。
- 宿主不能根据任意自定义 `state_file` 递归删除磁盘目录。清理插件数据只允许经校验的插件自有路径，并显式确认；CPA 凭证默认永不随插件卸载删除。
- 明确卸载处理的是哪个版本/全部受商店管理的版本，避免只删一个文件后残留其他候选造成显示混乱；未知手动安装文件不得随意清理。

### 4.3 修复插件生命周期：这是热升级的前置条件

当前宿主会调用 `plugin.quiesce`，插件却返回 unknown_method；宿主仍替换并将旧实例放入 retired 列表。新旧实例因此可能各自运行 worker，写同一 `state.json`。

插件需补齐以下契约：

```text
register → configured → ready
                     ↘ degraded
ready → quiescing → quiesced → reconfigure/resume 或 shutdown
```

实施要求：

1. 实现 `plugin.quiesce`：停止接收新的探测/发现/迁移任务，取消可取消任务。
2. worker、交叉验证 goroutine、手工探测任务、状态 flush timer 都必须被追踪；不能只取消主 ticker。
3. 等待所有可能写状态/凭证的任务结束，再完成最后一次 Flush；确认无残余写者后才返回停写成功。
4. 不持有生命周期大锁或宿主 apply 锁去等待仍可能回调 Host 的任务；锁顺序、回调和 drain 必须做死锁测试。
5. 取消需传到 HTTP 请求/扫描循环；正在 C ABI 回调里的操作不能假定可强制取消。超时则返回不能安全热切换，保留旧实例并提示重启。
6. `reconfigure` 同路径、配置未实质改变时复用状态与 worker；发生改变时先校验候选，再有序停旧/切换，不先覆盖全局 `store`。
7. 通过生命周期互斥与原子运行上下文管理 `store/currentConfig/workerCancel`，避免并发管理请求拿到不同代的对象。
8. Shutdown 幂等，并等待任务和刷盘完成；模块映射是否释放由宿主平台安全策略决定，不能把强制 `dlclose` 当作停 worker。

**旧版本首次升级特别处理：**

新版本增加 quiesce，并不能让已经运行的 `1.1.0` 自动学会停写。首次迁移必须由宿主识别“不支持安全停写”，转成受控重启，或在经过验证的宿主停旧机制下切换。对于当前旧实现，**优先选择停 CPA → 备份/替换 → 启 CPA**，不承诺首跳零停机，也不要求卸载。

### 4.4 安装、激活、回退形成完整闭环

在现有异步 reload 和 runtime rollback 上补齐，不重写一套无关加载器：

```text
校验来源/版本/平台
  → 下载到 staging 并验证 checksum
  → 保存操作记录与旧配置/旧版本指针
  → 安全停旧
  → 加载候选、注册、检查本地 ready
  → 原子提交目标配置/活动版本记录
  → 发布完成状态
失败 → 停止候选 → 恢复旧配置与旧运行状态 → 报告 rolled_back/failed
```

重点约束：

- 每插件串行变更，操作带 ID 和 generation；重复点击同一请求返回已有操作，避免并行安装/卸载。
- 配置修改使用“候选副本 → 校验 → 落盘成功 → 发布内存配置”；失败需补偿内存和文件指针，不能只有前端 toast。
- 宿主 `SaveConfigPreserveComments` 也要改成先完成编码、再安全持久化，保留注释与未知键；补磁盘满/短写/关闭失败测试。配置为 Docker 单文件 bind mount 时，直接 rename 覆盖挂载点可能受限，必须验证部署方式并提供明确的兼容保存策略，不能把普通文件的原子替换方案未经验证套上去。
- 不假设多个文件存在跨文件原子事务：使用小型操作记录描述阶段，启动时恢复未完成操作。
- 当前 Host 热替换主要依赖路径变化。同版本修复需使用可区分内容的新加载身份；身份包含插件 ID、版本、SHA256，并与宿主发现/命名规则兼容，不能随意改文件名后让扫描器失认。
- 普通可恢复故障允许有限重试并退避；ABI/平台错误不无限重试；panic fuse 的解除必须通过受控修复，不通过随意切开关伪装成功。
- 初次注册只有得到有效结果才标记 registered；Reconfigure 失败应保留上一次可用配置/能力快照，不让一个错误配置直接抹掉旧能力。
- 新增 `GET /v0/management/plugin-operations/:operationId` 查询异步操作；旧安装响应的 installed 语义保留，但新版 UI 继续等待 ready/failed/restart_required，不只检查 configured。

### 4.5 跨重启回退与版本保留

当前运行时可回退旧实例，但冷启动不具备同样保障，且旧文件清理没有 last-known-good 规则。

建议：

- 保留当前版和至少一个**已验证可用版**，在宿主支持的版本目录/清单中记录，不依赖最高版本猜测。
- 修改 `cleanupUnselectedPluginFiles`：保护活动版、回退版、未完成操作引用的文件；再按数量/空间清理其他受管理文件。
- 固定版本缺失时显示具体缺失版本；不得无提示自动跑未知高版本。
- 回退动作必须恢复持久化 manifest/目标版本，否则下次重启又选坏版本。
- 状态 schema 可向后兼容时才允许直接回旧二进制；不兼容时用升级前状态快照或禁止自动降级。
- 自动恢复旧快照必须发生在候选未接受新的业务变更之前；新版本已运行一段时间后再回滚，应预览数据差异，不能静默回退丢掉新节点。

### 4.6 状态文件不可静默丢失或替换

保留现有“节点敏感变化立即落盘、观测统计延迟合并”的方向，补充可靠性：

- `newStateStore` 返回加载状态/错误；明确区分不存在、无权限、JSON 损坏、schema 不兼容。
- 只有确认是全新安装才允许创建空状态。重启时已知路径突然缺文件，默认进入诊断，不自动当新安装。
- YAML 配置解码失败：保留上一份有效配置，显示字段错误；首次无有效配置时提供诊断管理入口，不静默换用默认状态路径。
- 指定路径不可写时不退到另一个空目录。自动路径仅供首次选择，选定后稳定记录并在 UI 显示；临时目录必须显著告警，不作为正式部署默认方案。
- 损坏文件保留原件，禁止后台覆盖；提供“恢复最近备份”和“明确重新初始化”两个不同操作。
- 唯一临时文件 → 写入 → Sync/关闭 → 平台安全替换；成功后再清 dirty。失败保持 dirty、记录 last_persist_error 并有限重试。
- 批量写失败时回滚候选内存状态；不能返回错误却留下一部分已经修改的内存节点。
- 升级前保留有限数量快照；代理 URL 仍需保存在私有状态中，文件和备份按敏感材料管理，Unix 最小权限保持 `0600`。
- 对旧 policy schema 的迁移不再无提示覆盖用户显式布尔选择；当前 `normalizePolicy` 的某些迁移会强制改交叉验证开关，需提供迁移差异并优先保留显式配置。
- `rotatable_node_ids` 依赖稳定节点 ID，普通升级/修复绝不能重编号。

---

## 5. P1：从现有 Grok 凭证自动发现并初始化节点

### 5.1 产品交互

空节点页面优先展示：

```text
你已在 CPA 中为 Grok 凭证配置代理？
[从 Grok 凭证发现节点]  [手动添加]  [批量粘贴]
```

已有节点时仍提供“发现新增节点”，用于后续补充，而不是只能第一次使用。

建议默认流程：

1. 打开页面触发**只读候选检查**，展示有可导入代理的提示；完整扫描异步执行，不堵注册。
2. 用户点击发现，查看扫描进度。
3. 预览：凭证数、唯一代理数、可新建数、已存在数、跳过/失败/冲突数。
4. 确认后一次性导入选中候选；默认只建立节点记录和归属映射。
5. 完成后进入分页节点列表；另行选择是否启用主动探测、隔离和迁移。

可选进阶开关 `auto_discover_on_start` 默认为 false。即使开启，也只在 ready 后异步执行 add-only 发现，不能在 `plugin.register` 中扫描全部凭证。

### 5.2 数据流与现有能力复用

```text
CPA host.auth.list
  → 严格识别 provider/type 为 xai 的凭证
  → host.auth.get(auth_index) 读取物理凭证 JSON
  → 取 proxy_url，验证并去重
  → 与已存在节点比对
  → 预览候选（无密钥）
  → 幂等 upsert 节点，不调用 host.auth.save
  → 更新派生绑定计数
```

- 不连接外部 Grok2API，不要求用户再次导入令牌。
- 优先复用 `auth_bind.go` 的 Host 调用和缓存，但单独提供“严格新鲜扫描结果”，不能直接把 `listAuthFilesFresh()` 返回值视为完整扫描。
- 当前 `listAuthFilesFresh()` 在错误时可能回旧缓存、逐条 get 失败又可能被略过。向导必须返回 `complete/stale/failedCount`，不能把失败解释成“没有凭证”。
- 当前宿主只支持 `auth_index`。插件用 `name` 作为兼容后备在本地宿主并无该契约；不能依赖名称假装稳定 get 成功，应能力探测或重新解析真实 index。
- 凭证筛选以明确 provider/type 为依据；只有缺少元数据时才使用兼容文件名规则，不用任意包含 `xai` 的文件名作为唯一判断。
- `runtime_only`、没有物理 JSON、缺 index 的凭证应明确列为当前不支持/跳过；未来有安全摘要能力后再扩展。

### 5.3 去重、命名和代理身份

**不要按出口 IP 去重。**多个代理可能共享一个出口 IP，同一代理也可能换出口；出口 IP 是观测值，不是节点身份。

V1 推荐保守规则：

- 对合法代理 URL 做首尾空白清理，以完整字符串计算内部 `proxy_identity_v1`。
- 用户名、密码和供应商会话参数都参与身份判断；不能只按 host:port 合并，否则会把不同 sticky session 合成一个节点。
- `socks5` 与 `socks5h` 不合并；不对路径、query、凭据大小写做猜测性归一化。
- 完整 URL 保持原样用于连接；ID/hash 只作内部索引，不在日志或前端输出可反推的敏感身份。
- 如未来引入协议/主机名规范化，必须同步修改归属计数、usage 映射、迁移等所有比较路径，并验证等价性；第一期不做激进规范化。
- 节点名称默认 `Grok 节点 0001` 等不含凭据的名称。预览用临时 candidate ID；有需要再展示脱敏主机，不暴露 userinfo/query。
- 已有同代理节点只关联计数，不覆盖节点名、容量、启用状态、隔离状态、历史和手工标记。
- 已有多个节点指向同一代理时列为冲突，不静默选一个、不自动合并历史；后续提供独立合并向导。

数据库式唯一约束在这里表现为 store 锁内检查 `proxy identity → node ID` 索引。预览去重之外，提交仍必须再次去重，防止并发导入产生重复节点。

### 5.4 不扰动现有账号与流量

初始化必须满足：

- 不调用 `host.auth.save`；不改 `proxy_url`、disabled、priority、token、账号标签。
- 默认统计所有可读取的 xAI 凭证，停用凭证也能作为节点发现来源，但仍保持停用；UI 分别显示启用/停用绑定数。
- 缺少 `proxy_url` 的账号计为“未配置单账号代理”，不猜测环境变量或全局代理、不自动建“直连节点”。
- 不推断“当前绑定数就是容量”。新节点容量默认沿用现有 `0 = 不限制` 的含义，允许用户在预览时统一指定；已有节点容量不变。
- 默认 `proxyPool=false`，不能从 URL 猜测它是否住宅轮换池。
- 不自动质量探测、不开启换 IP Webhook、不自动重平衡。

**需要额外防止间接改凭证：**当前发现节点后如果立即参与 hybrid 守护，后台可能隔离并迁移账号。因此新增节点应处于 `observe`（仅观察）模式，只有用户明确开启自动处置才进入 `manage`。

建议节点增加 `management_mode: observe | manage`：

- 旧节点迁移默认 manage，保持已有行为。
- 从凭证新发现的节点默认 observe。
- observe 可累计观测，不触发 quarantine/migrate/disable/rotation，也不由拦截器因新观察结果阻断账号。
- 是否进行有额度成本的主动探测单独确认，不能与只读发现捆绑。

### 5.5 初始化边界规则

| 场景 | 行为 |
|---|---|
| 1000 个凭证、1000 个不同代理 | 一次向导完成，无需手工录入 1000 次 |
| 1000 个凭证、20 个代理 | 最多新增 20 个节点，绑定数按实际统计 |
| 再次执行 | 已存在节点不新增、不重置策略 |
| 部分凭证读取失败 | 显示成功/失败清单；默认阻止自动提交，用户可明确选择“仅导入已确认候选” |
| 无效 URL、空代理、非 xAI | 分开计数并说明原因，不中断其他候选扫描 |
| 预览后凭证代理被修改 | 提交校验快照或重验候选，过期返回冲突要求刷新 |
| 预览后已有同代理节点被手工添加 | 提交时合并为 already_exists，不生成重复 |
| 状态文件损坏/历史状态缺失 | 进入恢复流程，不能凭“空节点”自动初始化覆盖历史 |
| 用户手动删除自动发现的节点 | 默认不会启动即重新加回；增量同步模式需记录忽略身份并支持显式恢复 |
| 用户从 CPA 删除凭证 | 节点标记无绑定/孤立，不自动删节点和历史 |

### 5.6 拟议插件 API

继续使用现有外层入口：

```http
POST /v0/management/grok2api-egress/api
Authorization: Bearer <existing management key>
X-Grok2API-Egress-UI: 1
Content-Type: application/json
```

以下均为**拟新增的内部 path**，不是独立裸露 HTTP 路由：

| method/path | 用途 |
|---|---|
| `POST /nodes/discovery` | 创建扫描任务，立即返回 operationId |
| `GET /operations/:id` | 进度、阶段、摘要、脱敏错误 |
| `GET /nodes/discovery/:id/candidates?page=1&pageSize=50` | 分页预览候选 |
| `POST /nodes/discovery/:id/commit` | 提交候选或显式选择全部已确认候选 |
| `POST /operations/:id/cancel` | 取消扫描；提交阶段遵循事务边界 |

提交示例：

```json
{
  "method": "POST",
  "path": "/nodes/discovery/discovery-id/commit",
  "body": {
    "selection": {"mode": "all_confirmed"},
    "expectedConfigRevision": 12,
    "idempotencyKey": "opaque-request-id",
    "newNodeDefaults": {
      "accountCapacity": 0,
      "proxyPool": false,
      "managementMode": "observe"
    }
  }
}
```

返回创建/已存在/跳过/失败数量及对应原因；不返回 token、完整代理或原始凭证 JSON。

事务和规模建议：

- 扫描可采用有界并发，初始上限建议 4，可配置并通过压测确定；不可对 1000 凭证直接无界 goroutine。
- 不持有 store 锁调用 Host。
- 1000 节点的首次提交可对候选整体校验后，在 store 内构建候选状态，执行**一次持久化和一次发布**，保证全有或全无。
- 为此新增内部 `upsertDiscoveredNodes`，不要循环调用现有 `createNode` 写盘 1000 次，也不要悄悄取消公共 `/nodes/import` 现有每批 500 上限。
- 初期为发现任务设置清晰的扫描数、候选数、响应大小与任务 TTL 上限；例如先覆盖 10000 凭证/候选的压力验收，再调整产品限制。
- 若以后改成分批提交，必须显式提供部分完成进度与恢复语义，不能继续宣称整次原子。
- `configRevision` 只随配置/节点结构变化增加，不因每次统计观测增加，避免长扫描永远提交冲突。
- 任务重试返回原结果；进程中断后通过已提交身份去重恢复，候选原始代理只在服务端保存，过期清理。

---

## 6. P1：真正可用的千节点分页

### 6.1 用户界面

建议默认每页 50 条，可选 20 / 50 / 100，不默认提供“全部”。

```text
[搜索名称 / ID / 出口IP] [状态] [启用情况] [来源] [排序]

☐ 选择本页   已选 68 个（本页 18 个）   [清空选择]
节点表格 / 移动端卡片

共 1000 个节点 · 匹配 128 个 · 第 2 / 3 页
[首页] [上一页] [页码/跳转] [下一页] [末页]  每页 [50]
```

规则：

- 搜索、过滤先于分页；筛选或页大小改变回第一页。
- 删除末页最后一条后回退至有效页；刷新保留页号、过滤条件、选择和滚动位置。
- 查询条件/页大小可本地保存，但不得保存管理密钥、代理 URL、凭证正文到新增缓存。
- 兼容现有正常→隔离→停用排序；提供显式排序选项，数字 ID 用数值语义加稳定 ID 次排序，避免 `1,10,100,2`。
- 自动刷新导致排序变化时，正在多选/编辑期间不偷偷重排当前操作对象；提示“有更新”或维持当前快照直到用户刷新。
- 桌面和移动端使用同一页模型，只渲染当前断点需要的视图。
- 复用现有账号分页的页码夹紧/边界逻辑和样式，不引入新前端框架。

### 6.2 两步交付，不停留在“假分页”

**P1a：前端快速改善。**

在现有全量数据上增加 filter → sort → slice，先解决 1000 行长滚动与 DOM 构建。明确这是过渡：网络和冷缓存成本尚未降低。

**P1b：服务端分页作为完整目标。**

拟扩展现有 `GET /nodes`：

```text
/nodes?page=1&pageSize=50&q=west&enabled=true&state=quarantined&sort=id&order=asc
```

示例响应：

```json
{
  "data": {
    "items": [],
    "total": 128,
    "page": 1,
    "pageSize": 50,
    "totalPages": 3,
    "configRevision": 12,
    "observedAt": "RFC3339 timestamp",
    "summary": {
      "totalNodes": 1000,
      "enabledNodes": 980,
      "quarantinedNodes": 6
    }
  }
}
```

约定：

- `total` 是当前过滤结果总数；summary 是全局数量，不能按当前页算。
- API 参数白名单：页号正整数，pageSize 上限 100，排序字段枚举；无效参数返回明确 400。
- 空结果 `items=[]`、`total=0`、逻辑当前页 1；页越界可返回夹紧后的 page，前端采用服务端结果。
- 第一阶段使用页号分页：1000 节点可在内存中过滤/排序，不必引入数据库。后续大量动态数据再评估 cursor。
- 不带分页参数时保留旧全量协议供旧客户端使用，标记 legacy；新版 UI 必须传分页参数。若要废弃全量模式，另行版本化，不突然截断旧 API。
- 已有 `data/items/total` 双层兼容字段短期保留；只序列化一次 DTO 的派生结果，避免重复计算。新协议可逐步统一。

### 6.3 状态与列表必须一起改

不能只改 `/nodes`。当前 `/quality-guard` 还带全节点 map、authStats，且 UI `nodeGuard()` 依赖这个 map。

推荐增量接口：

- `GET /status?view=summary`：插件运行信息、策略概览、全局计数、存储/worker 健康，不带全节点/全账号。
- 当前页 `/nodes` 自带这一页的 guard 字段，`nodeGuard()` 改用节点 DTO；不再依赖另一份全量 map。
- 账号面板打开时调用分页 `GET /auth-stats?page=...`，保留已有 20 条交互作为默认，不与节点页共享全量响应。
- 绑定弹窗扩展 `/nodes/:id/accounts?page=...&pageSize=...&q=...`。
- 配置和探针方案首次打开/修订号变化时再取；事件短期维持已有最近 100 条即可，筛选需求再加分页接口。

### 6.4 跨页选择与批量操作

- 表头复选框只选择本页；明确显示“本页”和“全部已选”。
- 以节点 ID Set 存储跨页选择，**不能继续用当前页列表清理所有不在页上的 ID**。
- 只有服务端明确确认删除/不存在的节点才移除；操作前重新校验有效 ID 和权限。
- 第一版仅支持逐页累积选择即可满足主要需求。若增加“全选所有匹配结果”，采用服务端查询快照/selection token，冻结范围并允许排除 ID，不能让新加入的节点被动态误选。
- 批量确认展示实际节点数、绑定账号影响数、停用/隔离节点数。
- 完全失败保留全部选择；部分成功仅清除成功项；返回失败项及重试入口。
- 并发修改使用 revision 校验，冲突重新预览，不对已经改变代理的节点继续旧操作。

### 6.5 轮询、渲染和慢请求

保留现有 15 秒刷新、可见性暂停和 loading 防重入，在此基础上：

- 仅取当前面板及当前页数据；恢复可见时立即补刷，连续错误指数退避。
- 搜索建议 200ms 去抖，变化时取消旧请求或用请求序号丢弃旧响应。
- 每个弹窗携带 nodeId+请求序号，防止先点 A 后点 B 时 A 的慢响应覆盖 B。
- pending 状态按 `nodeId + action` 保存，不只禁用旧 DOM 按钮；轮询重建也不会重新放开操作。
- 操作完成后的刷新应排队或等待正在进行的 load，不能因 `state.loading` 而直接丢弃。
- 模块请求独立错误处理，保留最后一次数据并显示陈旧时间；不要一个状态接口失败就把所有面板清空。
- 只对当前页渲染，避免每次勾选重建全表；尽量保留键盘焦点和已打开菜单。
- 分页完成后再测是否需要虚拟列表。1000 节点每页 50 条通常无需同时引入分页和虚拟滚动两套机制。

---

## 7. 其他值得一起规划的优化

### 7.1 安全的删除、换代理和重平衡

当前删除会把关联凭证 `proxy_url` 清空，并且忽略部分写入错误；这不只是 UI 体验问题，清空代理可能让流量改走全局代理/直连。

建议默认行为改为：

- 无绑定节点可直接删除。
- 有绑定节点先预览，默认拒绝直接清空凭证代理；提供“仅移除管理记录，保留凭证代理”和“先迁移到指定健康节点”两种明确选择。
- 只有用户明确选择“清除代理绑定”且确认流量影响时才允许清空。
- 节点换代理不能只改 node.ProxyURL：原凭证仍指向旧代理会导致归属丢失。提供“替换节点并迁移已绑定账号”的独立计划/执行流程，保留失败清单。
- 重平衡提供 dry-run，显示旧→新节点、迁移数量和容量缺口；默认尊重停用账号、手工锁定、observe 节点和容量。
- 当前重平衡满容量后会堆到最后一个节点，自动迁移也没有统一容量约束；改为容量不足明确失败/部分成功，不静默超卖。
- 对凭证更改使用预期旧 proxy/disabled 或版本校验；宿主支持时提供字段级 CAS/PATCH，避免保存旧 Raw JSON 覆盖同时发生的 token 刷新。
- 当前浅拷贝 authFile 仍共享 Raw map，`setAuthProxyAndFlags` 会改该 map；后续变更前复制原始对象，只有 save 成功后发布缓存，避免失败写入污染缓存或并发 map 访问。
- 原有“先摘除受影响账号，再迁到近期验证健康且不同出口 IP 的节点”防护应保留。迁移/验证失败的账号继续隔离，不能为了显示成功而直接启用。

### 7.2 批量任务与结果正确性

统一复用一个轻量插件 operation 机制处理发现、批测和大批迁移，不引入外部队列系统：

- 提交立即返回操作 ID，UI 展示 pending/running/succeeded/partial_failed/failed/cancelled。
- 每项输出明确结果，HTTP 200 不等于每个节点成功。
- 支持仅重试失败项；同节点相同探测进行中时返回已有任务，不重复消耗额度。
- 取消在每个安全边界生效；已经完成的迁移不会自动反向恢复，取消反馈必须说明已完成数量。
- 高风险任务记录发起时间、范围和结果；日志只存 ID 与脱敏原因。

### 7.3 请求热路径不要扫描所有凭证

当前注释称 `resolveNodeIDForAuth` 只查内存，但它会调用 `refreshAuthProxyCache`，后者可能进入过期 auth list 的 Host 扫描。

建议：

- 调度/拦截/usage 热路径读取已发布的不可变快照，不同步刷新 Host 数据。
- 独立后台构建 `auth identity → proxy identity → node ID` 与 `node ID → auth summary` 索引，完成后原子替换。
- `nodeIDByProxy` 当前 O(nodes) 遍历改成索引；写节点时维护唯一性。
- 热路径缓存过期使用最后成功快照并触发异步刷新；已有隔离记录不能因刷新失败被抹掉。
- 对完全未知凭证采用明确策略：第一版保持“不自动改账号、记录未映射告警”；若将来提供 fail-closed，必须作为显式选项并测试不可用风险。
- 管理操作要求严格快照；允许热点读旧缓存不意味着允许凭旧缓存执行删除/迁移。
- CPA 后续增加受权限控制的分页/批量代理摘要 Host API，只给需要的 provider/index/proxy/disabled/revision，不把认证 token 返回插件。该能力不能顺带泄漏到公开管理列表。
- 当前宿主按 index get 还会扫描 manager.List，N 次 get 存在更高总成本；优先完善宿主索引/批量摘要，而不是仅把 1000 个请求并发化。

### 7.4 千节点探测调度与成本

当前常规主动探测每 30 秒 tick 至多探测一个，1000 个节点若能均匀轮到，光 tick 间隔就约 **8 小时 20 分钟**；实际还叠加探测耗时及隔离复测。由于每次从排序前部寻找过期节点，前部节点重新到期后还可能抢占后部节点，不能保证公平覆盖。

推荐：

- 按 `next_due_at` 排队并使用轮转/公平调度；隔离复测独立优先级，但不能长期饿死普通节点。
- 连通探测与真实模型质量探测分开并发预算；初始建议连通 8、质量 2，再根据环境调整。
- 同节点单飞；全局每分钟任务数、Token 预算和失败退避均可配置。
- 初始化不自动排 1000 次模型探测。先展示覆盖周期与预计成本，用户选择抽样、分批或全部。
- 所需并发可按 `节点数 × 平均探测秒数 / 目标覆盖秒数` 粗估，再加入流量/额度限制；UI 显示预算无法满足目标周期的告警。
- 使用可取消请求与有界任务队列；不要为升级安全而给正常模型代理流量新增无关总超时。
- 未检测不等于健康。迁移目标仍需满足现有真实探测有效期与不同出口要求。

### 7.5 质量判断可解释性

当前具备 TPS、thinking、预期标记、主动/被动来源及账号错误分类，应增强说明，而非再堆不透明阈值：

- 每次异常显示证据类型、Token 数、生成时长、来源、方案和触发次数。
- 区分代理网络故障、认证失效、账号额度、上游错误；保留已有账号错误不消耗出口错误次数的规则。
- TPS 和缺 thinking 都是信号，不是模型质量的绝对证明；明确低样本、短响应、模型能力差异。
- 自动处置模式切换需确认，支持 observation-only 验证一段时间后再开启隔离/迁移。
- 统计面板显示样本量，避免把一次样本的 100% 降智率当成确凿账号故障。

### 7.6 诊断与备份

新增脱敏诊断包，包括：

- CPA/插件/CPAMP 版本、平台/ABI、插件文件身份、目标/活动版本。
- 配置键名和状态路径可写性、持久化错误、schema、节点数量、worker/缓存更新时间。
- 最近升级/发现/批量操作结果与脱敏错误。

不得包含管理密钥、token、完整代理 URL、Webhook Bearer Token 或原始 auth JSON。

“脱敏配置导出”和“完整可恢复备份”必须分开：前者不能恢复代理，后者含敏感数据，需要受保护的管理员下载或现有服务器备份机制，不承诺把掩码 URL 导入后就能恢复。

---

## 8. 数据结构与兼容迁移

### 8.1 建议新增字段

| 位置 | 字段 | 作用 |
|---|---|---|
| 插件状态根 | schema version、config revision、last successful persistence | 迁移/并发校验/诊断 |
| 节点 | origin：manual/credential_discovery | 区分来源，不覆盖手工配置 |
| 节点 | management_mode：observe/manage | 安全接入已配置的凭证 |
| 节点内部 | proxy identity / 索引 | 去重与 O(1) 映射，不对外暴露秘密 |
| 发现状态 | last scan summary、ignored identities | 增量发现与手动删除抑制 |
| 宿主插件记录 | desired/active/last-known-good、operation stage | 升级和冷启动恢复 |

绑定数、健康计数、节点反向索引优先作为派生数据，不复制一套独立“账号数据库”。

### 8.2 迁移原则

1. 升级前先备份，校验读取成功后才迁移；不得读失败后创建空 state 覆盖。
2. 原节点 ID、代理、名称、容量、策略、探针、隔离和白名单引用保持不变。
3. 旧节点缺 origin 默认 manual，缺 management_mode 默认 manage；新发现节点明确 observe。
4. 新字段缺失填默认；用户显式配置优先。布尔字段用可区分“未提供”和 false 的解析方式。
5. 记录 from/to schema，迁移可重复执行；失败不提交部分状态。
6. 不改现有插件 ID、默认状态目录名和菜单资源路径，不因品牌整理引起配置失联。
7. 发布前验证旧版本状态 → 新版本 → 重启，并在明确的 schema 支持范围内验证回退。
8. 新增 Host 能力必须检测兼容性；旧宿主不支持时使用受限兼容扫描或明确提示升级，不伪装已支持批量 API。

---

## 9. 分期交付与具体改动范围

建议按以下 PR/工作包拆分，每包可独立审阅，不一次性重写整个插件。

| 工作包 | 优先级 | 主要文件（已有文件优先） | 完成条件 |
|---|---|---|---|
| A. 配置与状态保护 | P0 | 插件 `go/main.go`、`go/store.go`、`go/main_test.go` | 加载错误可见、不空状态覆盖、稳定路径、失败保留 dirty、迁移保留用户配置 |
| B. 生命周期安全 | P0 | 插件 `go/main.go`、`go/guard.go`、`go/auth_bind.go`；CPA `internal/pluginhost/host.go` 与相关测试 | quiesce/await/flush、无双写、旧版本首跳受控重启、rollback 可恢复 worker |
| C. 保留配置修复和激活结果 | P0 | CPA `plugins.go`、`plugin_store.go`、`server_management.go`、`internal/config/config_yaml.go`；CPAMP `PluginsPage.tsx`、`PluginStorePage.tsx`、`pluginPolling.ts`、安装确认组件、API/types/i18n | 不用 DELETE 进行修复；安装与生效分开；展示错误/进度；旧接口兼容 |
| D. 节点前端分页 | P1a，可与 A–C 并行 | 插件 `go/page.html`，复用 `accounts-panel.js` 分页约定 | 千节点不再全量 DOM；本页选择、页码/末页回退正确 |
| E. 凭证发现向导 | P1 | 插件 `go/auth_bind.go`、`go/store.go`、`go/main.go`、`go/page.html` | 预览、去重、严格扫描、幂等提交、observe，不写凭证 |
| F. 后端分页与安全批量 | P1b | 插件 `go/main.go`、`go/store.go`、`go/page.html`、`go/accounts-panel.js` | summary 与页面拆分；跨页不丢选择；绑定弹窗分页；失败准确反馈 |
| G. 回退/发布验收 | P1 | CPA `internal/pluginhost/platform.go`、商店安装相关测试；`.github/workflows/cpa-plugin-release.yml` | LKG 保留、重启恢复、真实宿主双架构烟测与兼容矩阵 |
| H. 调度/缓存规模优化 | P2 | 插件 `go/guard.go`、`go/auth_bind.go`、`go/store.go`；必要时 CPA auth callbacks/SDK | 热路径无 Host 扫描、公平探测、预算/任务取消、可观测性 |

实现发现任务/分页时可按责任新增少量 Go 文件，避免继续把全部逻辑塞进 `main.go`；但不为本方案新建外部服务、数据库、通用插件框架或一套新 UI 技术栈。

### 9.1 建议发布安排与粗估

- **第一批维护版**：A/B/C，加上 D 的分页快速改善。解决“升级风险”和“下拉太长”。
- **第二批功能版**：E/F/G，完成一键节点发现与真正服务端分页。
- **第三批规模优化**：H 与审计/趋势增强，依据实际压测排序。

单个熟悉三仓库的开发者粗估：A/B/C 约 5–8 工程日，D 约 1–2 日，E/F/G 约 6–10 日，H 约 4–7 日；不含上线窗口等待与复杂兼容性返工。生命周期/跨重启回退风险最高，应先做最小验证。版本号由维护者发布时决定，不将本文建议版本当成已发布版本。

---

## 10. 测试与验收清单

### 10.1 已有测试基础

插件现有 [main_test.go](./go/main_test.go) 可直接扩展：

- `TestConfigureAcceptsStoreInstallYAMLWithoutHostAuth`
- `TestConfigureFallsBackWhenYAMLIsGarbage`：目标行为改变后应改为验证“不静默切空配置/保留旧有效配置”，不能照旧保留错误预期。
- `TestStoreNodeCRUD`、`TestStoreCreateNodesIsAllOrNothing`
- `TestDispatchNodesList`、`TestDispatchNodesImportRedactsProxyURLs`
- `TestAuthListCacheAvoidsRepeatedHostGets`、`TestDebouncedPersistCoalescesStats`
- `TestMigrationFailsClosedAndVerifiesHostAuthSave`
- `TestRequestInterceptorRejectsQuarantinedAuth`
- `TestRenderStatusPage`：目前主要是字符串断言，应改用完整 `renderPageHTML` 校验内嵌资源，但字符串测试不能替代浏览器交互验收。

宿主已有 [host_test.go](../../CLIProxyAPI/internal/pluginhost/host_test.go)、[platform_test.go](../../CLIProxyAPI/internal/pluginhost/platform_test.go)、[plugin_store_test.go](../../CLIProxyAPI/internal/api/handlers/management/plugin_store_test.go)、[plugins_test.go](../../CLIProxyAPI/internal/api/handlers/management/plugins_test.go)，覆盖 Reconfigure、quiesce、热回退、固定版本、保留配置更新和删除等，可添加回归而不从零搭框架。

CPAMP 已有 [API 测试](../../CPA-Manager-Plus/apps/web/src/services/api/plugins.test.ts)、[轮询测试](../../CPA-Manager-Plus/apps/web/src/features/plugins/pluginPolling.test.ts)、[显示测试](../../CPA-Manager-Plus/apps/web/src/features/plugins/pluginDisplay.test.ts)，用于补齐状态解析、激活完成条件、超时与错误展示。

### 10.2 升级与数据完整性

- [ ] 正常更新前后宿主配置的自定义字段不变，store 版本字段除外。
- [ ] state 中节点 ID/代理/容量/策略/探针/隔离不丢失；凭证文件无非预期修改。
- [ ] 更新失败后旧版本仍可用，持久化目标版本一致；重启不会再次选坏版本。
- [ ] 缺二进制、错误架构、无权限、缺导出符号、ABI 错误均显示正确阶段。
- [ ] “已安装但未注册”不会出现成功生效 toast，操作超时不是静默成功。
- [ ] 同版本损坏包修复真正加载新内容，不只 Reconfigure 旧映射。
- [ ] 重装/修复网络失败不先删旧配置/旧二进制。
- [ ] quiesce 后无新任务/无刷盘/无 Host 写回，主 worker 与其他 goroutine 全部受控。
- [ ] 注册不调用 Host auth API；慢 Host 回调时 quiesce 不死锁，无法安全停止时提示重启。
- [ ] 连续升级/回退不累积活动 worker；旧 1.1.0 首跳走受控迁移，不冒充无缝热升。
- [ ] state 损坏、只读卷、磁盘写失败、rename 失败，不覆盖旧数据；重试和诊断有效。
- [ ] 容器重建分别保留 config/auth/plugins/plugin-data；工作目录改变不会悄悄选新状态。

### 10.3 凭证发现

- [ ] 1000 个独立代理一次发现/确认即可建节点；重复执行新增为 0。
- [ ] 多账号共享代理只建一个节点；不同用户名/密码/session/socks5h 语义不错误合并。
- [ ] 空代理、非 xAI、无效 URL、runtime-only、读取失败有独立计数。
- [ ] 有旧缓存但新扫描失败时明确 stale/incomplete，不当成功空扫描。
- [ ] 预览到提交期间账号改变、节点被添加、配置 revision 改变正确处理。
- [ ] 初始/重复/部分失败/取消/重启后重试都幂等。
- [ ] 初始化期间和之后的 observe 模式不触发任何 `host.auth.save`；停用账号始终停用。
- [ ] 候选列表、错误、日志、导出不含 token 或完整代理凭据。
- [ ] 保持节点 ID 稳定，rotation 白名单引用不失效。

### 10.4 分页与操作

- [ ] 0、1、20、21、50、51、1000 节点边界正确；筛选目标在原列表末尾也能找到。
- [ ] 分页前过滤/排序；总数与概览不受当前页影响。
- [ ] 翻页/刷新保留跨页选择；表头只选本页；不误操作未选项。
- [ ] 删空末页回退；部分失败保留失败选择和结果清单。
- [ ] 慢请求与 15 秒轮询不会重置进行中状态，不重复发起探测/删除。
- [ ] 绑定弹窗 A/B 响应倒序不串内容；桌面/移动布局一致。
- [ ] 已有账号统计每页 20 条和搜索行为不退化。
- [ ] 节点名/账号名/错误文本的 XSS 测试字符串按文本展示，不作为 HTML 执行。
- [ ] 删除有绑定节点默认不清空凭证代理；重平衡满容量明确报告，不堆到最后节点。

### 10.5 性能验收目标（实施时测量）

基准建议：记录 CPU/内存/OS/浏览器版本；使用 2 vCPU、4 GiB 级测试环境作为起点，准备 100、1000、10000 节点及可控 Host mock。代理真实网络测试与本地 API 测试分开。

| 项目 | 建议目标/判定 |
|---|---|
| 节点 DOM | 每个可见列表最多 pageSize 条；隐藏布局不重复生成全量数据 |
| 页接口 | 1000 节点、缓存已热时 P95 ≤ 200ms；记录返回字节数，不含外网探测 |
| 翻页/选择 | 桌面测试环境主要交互 ≤ 100ms，显著长任务需要定位 |
| UI 初始数据 | summary + 当前页，不夹带全部节点和全部账号统计 |
| 请求热路径 | 稳态和缓存过期路径均不直接调用 Host list/get；刷新在后台 |
| 1000 节点导入 | 一次状态提交，不 1000 次 fs write；峰值内存与耗时有记录 |
| 探测公平性 | 在预算足够时全部节点可在目标周期覆盖；预算不足明确告警，无末尾饥饿 |
| 生命周期 | quiesce 成功后写计数不再增加；连续升级活动写者始终不超过一个 |

网络扫描耗时不能无条件承诺固定秒数；用 `凭证数 × Host 单次耗时 / 有界并发` 估计，并展示进度/取消，不把“注册快”与“全量发现完成快”混为一谈。

### 10.6 建议执行的验证命令

以下是**实施代码改动后的建议命令**，本次文档交付未执行这些业务测试。

在具有 CGO 编译器的目标 Linux/CI 环境、插件 `cpa-plugin/go` 目录：

```sh
go test ./...
go test -race ./...
go build -buildmode=c-shared -o grok2api-egress.so .
```

在 CPA 仓库：

```sh
go test ./internal/pluginhost ./internal/pluginstore ./internal/api/handlers/management
go build -o cli-proxy-api ./cmd/server
```

在 CPAMP 仓库，使用现有 npm workspaces：

```sh
npm --workspace apps/web run test -- src/services/api/plugins.test.ts src/features/plugins
npm --workspace apps/web run type-check
npm --workspace apps/web run build
```

额外集成门禁：使用发布包在明确支持的 CPA 最低版本和当前版本上跑安装→配置→重配→升级→回退→重启；amd64/arm64 都有真实加载烟测，不只交叉编译。固定支持的 glibc 基线，避免 `ubuntu-latest` 构建环境漂移导致发布包在较旧宿主镜像加载失败。

---

## 11. 改造上线前的安全操作建议

这不是已经存在的“一键修复功能”，而是当前版本的止损顺序：

1. **不要先卸载。**先记录 CPA/插件版本、宿主插件列表、配置项、当前 state_file 路径及相关 loader 日志；对外提供时必须脱敏。
2. 备份宿主 `plugins.configs.grok2api-egress`、插件状态目录、插件二进制和 CPA 凭证；不要把备份上传到仓库。
3. 核实三类持久化：宿主 config、插件二进制目录、插件状态目录；凭证目录也应继续保持原挂载。
4. 检查全局/单插件启用、固定版本是否存在、进程实际工作目录、权限、OS/arch/libc、ABI 错误。`已配置` 不能替代这些检查。
5. 有权限且需要替换当前旧版本时，安排受控停机，在 CPA 停止后做一致性备份/替换，再启动；不要连续点击卸载/重装。
6. 若历史 state_file 已丢失，先寻找原路径/卷/备份；不要马上建空节点覆盖。最后才用凭证发现恢复代理节点，并说明策略历史无法自动找回。

Docker 部署可在**现有 volumes 列表上追加**以下挂载（不要用这一行替换已有 config/auth/plugins 挂载）：

```yaml
volumes:
  - ./plugin-data/egress-guard:/CLIProxyAPI/plugin-data/egress-guard
```

并明确配置插件 `state_file` 指向同一容器路径，核对进程 UID/GID 可读写。状态目录名 `egress-guard` 是当前既有路径，不要误改成另一个空目录。

---

## 12. 本次文档交付与下一步

本次完成：

- 基于三个本地仓库核对实际安装/注册/删除、状态存储、凭证读取、节点 UI 和探测链路。
- 区分已确认缺口、已有防护和部署待查项，没有把可能原因写成已复现根因。
- 给出分期实施范围、兼容接口、数据迁移、安全约束和验收清单。
- 仅生成本 Markdown 方案，未修改业务代码、未安装/卸载插件、未访问用户线上实例。

**建议下一步批准第一批工作包 A/B/C/D。**先保证升级/修复不破坏数据，并立即解决节点长列表；再交付凭证自动发现和完整后端分页。若宿主暂时不能配合，插件可先做存储诊断、凭证发现与分页，但不能据此承诺彻底解决“未注册后的自修复”和旧版本安全热升级。
