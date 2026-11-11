# 开发文档

面向后续维护和扩展功能。使用说明见 [README](../README.md)。

## 技术栈与约定

- Go 1.24+，单个 `package main`，无框架，路由用标准库 `net/http`（Go 1.22 的方法 + 路径模式）
- 依赖只有两个：`gopkg.in/yaml.v3`（YAML）、`github.com/dlclark/regexp2`（正则）
- 前端是单个 `web/index.html`（原生 JS，无构建步骤），用 `embed` 打进二进制
- 存储是 JSON 文件，没有数据库

**为什么用 regexp2 而不是标准库 `regexp`**：机场用户常用的节点筛选正则大量依赖前瞻断言（如 `^(?=.*香港)(?!.*IPLC).*$`），标准库的 RE2 不支持。所有面向用户的正则都必须走 `compileRegex` / `optRegex`（带 2 秒匹配超时，防止灾难性回溯）。

**术语**：「订阅组」(`Profile`) 是本服务的概念，一个订阅组对应一个输出链接；「策略组」(`Group`) 是 Clash 的 proxy-group。代码和界面里注意别混。

## 文件结构

| 文件 | 职责 |
| --- | --- |
| `main.go` | 启动、路由、认证与会话、HTTP 处理函数、保存配置时的校验 |
| `transfer.go` | 配置的导出与导入（整份配置 / 单个订阅组） |
| `access.go` | 订阅链接的访问记录、客户端 IP 的判定 |
| `rulenames.go` | 规则来源的校验、从 GitHub 拉取规则清单、清单的保存 |
| `store.go` | 数据模型、JSON 持久化、旧配置迁移、密码哈希、默认配置与内置模板 |
| `fetch.go` | 拉取上游订阅（解锁、代理）、按链接 + UA 的缓存、订阅内容解析入口 |
| `uri.go` | 分享链接（ss / vmess / trojan / vless / hysteria2）→ Clash 节点 |
| `build.go` | 核心流程 `Build`：决定拉取用的 UA → 合并节点 → 策略组 → 规则 → Parser → 校验 → 输出 YAML |
| `parser.go` | CFW Parsers 语法实现、正则封装 |
| `web/index.html` | 管理界面，以及 UA 预设 |
| `app_test.go` | 测试：命令语法、Parser 顺序、分享链接、端到端构建、UA 透传、上游走代理、迁移、配置校验（来源与引用）、内置模板一致性 |
| `transfer_test.go` | 测试：导出导入 |
| `access_test.go` | 测试：访问记录、客户端 IP |
| `rulenames_test.go` | 测试：规则清单的拉取与保存、规则来源的校验 |

## 数据模型

```
Config
├─ Version                 配置结构版本，见「迁移」
├─ Sources[]               节点来源（全局，被订阅组引用）
│    ├─ ID / Name
│    ├─ Kind = remote：URL、UAMode、UnlockURL
│    └─ Kind = manual：Nodes（YAML 或分享链接文本）
├─ Profiles[]              订阅组
│   ├─ ID / Name / Token / FileName
│   ├─ Sources[]           对节点来源的引用（SourceRef）：Source（来源 ID）、Enabled、Prefix、Include、Exclude、Dialer（前置代理）
│   ├─ Groups[]            策略组
│   ├─ Rules               RulesBefore / RuleSets[] / RulesAfter（内嵌结构体）
│   ├─ Parser
│   └─ Applied             模板类型 → {最近一次套用的模板 ID, 当时的内容摘要}
├─ Templates[]             模板：Kind = groups / rules / parser，带对应那部分内容
└─ 全局：BaseConfig、CacheMinutes、UpdateInterval、UserAgent（固定 UA）、UnlockUserAgent（解锁 UA）
```

**节点来源是全局对象，订阅组里只有引用。** 划分的原则：决定「从上游拿到什么」的属于来源本身（`Source`：链接、UA 模式、解锁地址、手填节点、名称）；决定「这个订阅组怎么用它」的属于引用（`SourceRef`：是否启用、前缀、Include / Exclude、前置代理）。同一个机场被三个组用，链接只存一份，改一处即可；同一个来源也可以在一个组里引用多次（比如按不同的正则拆成两批、加不同前缀）。因为缓存键里只有来源 ID，引用同一个来源的订阅组共用缓存，不会重复拉上游。

来源没有订阅组引用时仍然保留（在「节点来源」页显示「未被使用」），不会自动删除。`validateConfig` 会去掉指向不存在来源的引用。

**模板是复制语义**：套用模板 = 把模板内容复制进订阅组，之后两者互不影响。所以服务端只负责保存 `Templates`，「套用」是前端对 `cfg` 对象的操作（`applyTemplate`，`KINDS[kind].fields` 列出了复制哪些字段），`Build` 完全不知道模板的存在。模板只在「模板」页新建和编辑（三类都能从空白新建），订阅组里没有「存为模板」。内置模板（`seedTemplates`）只在新建配置或迁移时写入一次，之后和用户自建的没有区别，可以改、可以删。

内置的两套策略组模板和两套规则模板共用同一批策略组名（`gProxy` 等常量），所以可以任意组合；`TestSeedTemplatesAreConsistent` 保证这一点。新增内置模板时沿用这些名字。

### 模板与订阅组的差异提示

复制语义下，套用之后就分不清订阅组「还是不是模板原样」，模板改了也不知道哪些组没跟上。所以套用时在 `Profile.Applied[kind]` 记下模板 ID 和当时模板内容的摘要，界面据此在策略组 / 规则 / Parser 页显示：

| 订阅组现在的内容 | 模板现在的内容 | 提示 |
| --- | --- | --- |
| 与模板现在的内容相同 | | 来自模板「X」 |
| 与套用时不同 | 与套用时相同 | 来自模板「X」，已修改 |
| 与套用时相同 | 与套用时不同 | 模板「X」有更新，可重新套用（带「重新套用」按钮） |
| 与套用时不同 | 与套用时不同 | 来自模板「X」，本组已修改；模板也有更新（重新套用前会确认覆盖） |

只做提示，不自动同步（引用语义是明确不做的）。几个实现要点：

- 摘要完全由前端计算（`digest`），服务端只是原样保存 `Applied`，并在 `validateConfig` 里去掉指向已删除模板或类型不符的记录
- 摘要前先做规范化（`canon`）：键排序，去掉 `null`、空串、`false`、`0`、空数组。这样对象是前端新建的还是服务端返回的（键顺序不同、多了默认字段、`nil` 切片变成 `null`）不影响结果。代价是「把某个值从空改成 0」这类变化不算修改，可以接受
- 模板 ID 由前端按服务端的格式生成（`newID()`，16 位十六进制），这样新建的模板不用先保存就能被套用和记录；`validateConfig` 对格式正确且不重复的 ID 原样接受
- 提示是在 `toolbar()` 里画的，通过模块级的 `liveHint` 在每次 `markDirty()` 时刷新，所以输入过程中不用整页重绘也能即时变化
- 模板改名不影响摘要；模板被删除后提示消失，订阅组内容不受影响

## 构建流程

订阅链接 `GET /sub/{token}`（按 token 找到订阅组）和预览 `GET /api/preview` 都调用同一个 `Build(cfg, profile, fetcher, clientUA, force)`。`clientUA` 是来拉合并链接的客户端的 UA（预览时由 `?ua=` 指定）：

```
基础配置 (cfg.BaseConfig) → root map
  1. 并发处理本组所有启用的来源引用（按引用的来源 ID 在 cfg.Sources 里找到来源）
       remote：resolveUA 决定 UA → Fetcher.Get → parseSubscription
       manual：parseCustomProxies 解析 Nodes
  2. 汇总节点        按引用上的 Include/Exclude 过滤 → 加 Prefix → 重名加序号 → 引用上有 Dialer 的写上 dialer-proxy
  3. 生成策略组      Extra（固定成员）在前，按 Nodes 模式追加节点
  4. 生成规则        RulesBefore → 每个 RuleSet 一条 RULE-SET → RulesAfter
  5. ApplyParser     本组 Parser 对 root 做最终修改
  6. validate        检查引用了不存在的策略 / 成员 / 前置代理、前置代理成环，只产生警告
  7. toNode + 编码   输出键顺序稳定的 YAML
```

手动节点在第 1 步就进入节点列表，所以会参与策略组筛选，Parser 也能引用它们。

**前置代理（代理链）**：`SourceRef.Dialer` 非空时，这条引用下的节点都写上 `dialer-proxy`（mihomo 的字段，值是策略组名或节点名），表示这些节点经由它拨号，典型用法是「自建落地节点经由机场节点出去」。它属于引用而不是来源，所以同一个来源在不同订阅组可以用不同的前置，也不进缓存键；引用上填了就覆盖节点自带的 `dialer-proxy`，没填则保留自带的。没有做 `relay` 类型的策略组（mihomo 已弃用）。

前置代理的成员里不能有以它为前置的节点，否则客户端选中那个成员时节点就绕回自己。`Build` **不会自动把这些节点从策略组里去掉**，只由 `validateDialers` 报警告：顺着「策略组 → 成员」「节点 → 它的 `dialer-proxy`」走，能回到节点自己就算成环。这个检查在 Parser 之后做，所以手填节点自带的、Parser 命令写上的 `dialer-proxy` 也会被检查。

`BuildResult.Subs` 里每个启用的引用一条状态（`SubStatus`）：`Index` 是引用在订阅组 `Sources` 里的下标，`ID` 是来源 ID。同一个来源可能被引用多次，所以前端用「下标 + 来源 ID」对应到行上。

内存中的配置树统一使用 `map[string]any` / `[]any`（不是 `[]map[string]any` 或 `[]string`），因为 Parser 的路径命令要对任意位置做通用遍历。往 `root` 里放数据时请保持这个约定，否则 `applyPath` 和 `toNode` 不认识。

### 拉取上游用的 UA（`resolveUA`）

- **链接**：每个来源只有一个 `URL`。为空时该来源不提供节点（状态里报错，不影响其他来源）
- **UA**：`UAMode == "passthrough"` 且客户端带了 UA 时用 `clientUA`；否则用全局的固定 UA `Config.UserAgent`；再为空用 `defaultUA`。来源自己不带 UA，只选模式
- `clientUA` 在 `main.go` 里被截断到 256 字符

上游内容只认 Clash YAML 或分享链接列表，不按客户端去拉它的原生格式；输出格式也不随客户端变化，始终是 Clash YAML。这样设计的理由和以后支持其他客户端的做法见 [CLIENT-SUPPORT.md](CLIENT-SUPPORT.md)。

### 上游拉取与缓存（`fetch.go`）

- 每次拉取：先 GET `UnlockURL`（若有），再 GET 订阅地址。解锁只是一次普通的 GET——上游要求该地址被访问过之后才放行订阅——两次请求互相独立，不共享 Cookie。解锁请求使用全局的解锁 UA `Config.UnlockUserAgent`（为空用 `chromeUA` 常量，即 Chrome 的 UA），不随订阅的 UA 模式变化；订阅请求使用 `resolveUA` 给出的 UA。解锁请求只在网络失败时报错，不检查状态码
- 缓存键 = `来源 ID + sha256(实际链接 + 实际 UA + UnlockURL)` 的前缀（`cacheKey`）；有解锁地址时解锁 UA 也计入。键里没有订阅组，所以引用同一个来源的订阅组共用缓存。同一来源被不同 UA 透传拉取会各存一份；固定 UA 的来源不管哪个客户端来都共用一份。内存一份 + `DATA_DIR/cache/<key>.json` 一份，启动时全部读入内存
- 有效期是 `Config.CacheMinutes`（默认 5 分钟）。有效期内同一个键不会再请求上游
- 每个键有一把锁（`keyLock`），并发的相同请求只会拉一次上游
- 拉取失败后同一个键 30 秒内不重试（`failBackoff`），避免上游故障时每次客户端请求都去打上游；`?refresh=1` 可绕过
- 每个来源最多保留 16 份缓存（`maxEntriesPerSub`），超出淘汰最旧的。透传模式下缓存份数由客户端 UA 的种类决定，这个上限防止无限增长
- **失败时的回退**：`Fetcher.Get` 成功返回 `(entry, nil)`；失败返回 `(同一个键的旧缓存或 nil, err)`。**不会**回退到同一来源其他链接 / UA 的结果——不同 UA 拿到的节点可能不同，某个 UA 失败就是失败。`Build` 据此设置 `SubStatus.Stale`
- 修改来源的链接、UA 模式、解锁地址，或全局的固定 UA、解锁 UA 后，受影响来源的缓存键随之改变，旧缓存不会被命中（留在磁盘上直到被淘汰）
- 缓存经过 JSON 往返后整数会变成 `float64`，`toNode` 会把整数值的 float 还原成整数输出
- 来源 ID 被用作缓存文件名，所以 `validateConfig` 只接受 16 位十六进制且全局唯一的 ID，其他一律由服务端重新生成（防路径穿越）。来源的 ID 被重新生成时，订阅组里指向旧 ID 的引用会跟着改
- **代理**：只走环境变量 `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`，不做界面配置，也不按来源单独配置。`fetchSubscription` 的 `http.Client` 没有自定义 `Transport`，落在 `http.DefaultTransport` 上，它本身就读这几个变量（也支持 `socks5://`），订阅请求和解锁请求都经过它。启动时 `proxySummary` 把生效的代理打进日志（代理地址里的密码会被隐去）。**以后给拉取换了自定义 `Transport`，要带上 `Proxy: http.ProxyFromEnvironment`**，否则代理静默失效——`TestFetchUsesProxy` 会发现。两个标准库的行为要知道：回环地址（`localhost`、`127.0.0.1`）永远不走代理；环境变量只在进程第一次发请求时读一次，所以测试是在 `TestMain` 里设置的，且用一个不存在的域名当上游
- 订阅内容解析顺序：Clash YAML（有 `proxies`）→ base64 解码后的分享链接 → 明文分享链接

### 输出 YAML（`toNode`）

- 顶层键顺序：`headKeys`（端口、模式等）→ 其余按字母序 → `tailKeys`（proxies、proxy-groups、rule-providers、rules）
- 非顶层对象：`name, type, server, port` 在前，其余按字母序
- `proxies` 下每个节点输出成单行（flow style）
- 标量通过 `yaml.Node.Encode` 生成，所以 `"true"`、`"123"` 这类字符串会被正确加引号，不会变成布尔 / 数字
- yaml.v3 会把 emoji 输出成 `\U0001F680`，`unescapeRunes` 负责还原

## 存储

`DATA_DIR/config.json`：

```json
{ "auth": { "hash": "...", "salt": "..." }, "config": { ...Config... } }
```

- 写入是「写临时文件再 rename」，不会出现写一半的文件
- 密码：PBKDF2-SHA256，20 万次迭代。设置了 `ADMIN_PASSWORD` 时忽略文件里的哈希，直接比对环境变量
- 会话：随机 ID 存在进程内存里（`App.sessions`），有效期 7 天，**重启后需要重新登录**。Cookie 为 HttpOnly + SameSite=Strict
- 登录尝试被一把互斥锁串行化，失败后 sleep 1 秒，以此限制爆破速度
- 订阅组的 `Token` 只由服务端生成：`PUT /api/config` 时按订阅组 ID 找回原令牌，前端传来的 `token` 一律忽略；新订阅组（ID 未知或为空）分配新 ID 和新令牌。唯一的例外是整份导入，见下

`DATA_DIR/access.json`：各订阅链接的访问记录，见「访问记录」。`DATA_DIR/cache/`：上游缓存。`DATA_DIR/rulenames.json`：各规则来源的规则清单，见「规则来源」。这三处都不属于配置，删掉只是丢历史 / 重新拉取。

### 迁移

`Config.Version` 记录结构版本，`Config.migrate` 在 `OpenStore` 时把旧文件升到 `configVersion`，迁移前的原文件保存为 `config.json.bak`。

| 版本 | 结构 | 迁移到下一版时 |
| --- | --- | --- |
| v1（无 `version`，无 `profiles`） | 只有一组订阅，字段都在顶层 | 顶层的 token / fileName / subscriptions / customProxies 装进名为「默认」的订阅组，原订阅链接不变 |
| v2（无 `version`，有 `profiles`） | 多订阅组，但策略组 / 规则 / 通用 Parser 在顶层共用 | 共用的策略组和规则复制进每个订阅组；`customProxies` 变成一个 manual 来源；通用 Parser 存为模板，并填入 Parser 为空的订阅组（两边都有内容时无法自动合并，只保留为模板）；写入内置模板 |
| v3 | 策略组、规则、Parser 都在订阅组内；订阅带「专用链接」（`variants`，按客户端 UA 切换上游链接）和各自的固定 UA（`userAgent`） | 移除专用链接：默认链接为空的订阅改用第一条有链接的专用链接，其余专用链接丢弃。固定 UA 挪到全局：取第一个填了 UA 的订阅的值（与内置默认值相同则留空），其余丢弃 |
| v4 | 订阅（`subscriptions`）存在各订阅组里，来源本身和使用设置混在一个对象上 | 每条订阅拆成全局来源 + 订阅组里的引用（`splitSubscription`）。内容完全相同的来源合并成一个（远程：链接 + UA 模式 + 解锁地址；手动：节点文本），名称和 ID 取第一次出现的，这样原有缓存继续有效；没填链接 / 节点的不合并。前缀、过滤、是否启用留在各组的引用上 |
| v5 | 没有 `ruleSources`，规则来源是前端里写死的预设 | 写入内置的规则来源（`seedRuleSources`）。原有的规则集不动 |
| v6 | 当前结构 | |

旧字段以 `Legacy*` 命名、`omitempty` 保留在结构体里，只用于读取旧文件。以后改结构：`configVersion` 加一，在 `migrate` 里加一段（每一段只在 `from` 低于对应版本时执行），并在 `TestMigrateLegacyConfig` 补一个旧文件样例。导入功能复用同一个 `migrate`，所以旧版本导出的文件、旧的 `config.json` 都能导入。

只是**加字段**不需要迁移：旧文件里没有这个字段，读出来是零值，所以零值必须是合理的默认行为（或在 `validateConfig` / 使用处兜底）。例如 `UAMode` 为空等同于 `fixed`，`Kind` 为空等同于 `remote`，`Applied` 为空就是没有差异提示。

## 导出与导入（`transfer.go`）

两种文件，都是 `exportFile` 结构（`kind` 区分），都带导出时的 `version`：

| | 整份配置（`kind: config`） | 单个订阅组（`kind: profile`） |
| --- | --- | --- |
| 用途 | 备份、回滚、换服务器 | 分享一套配置 |
| 内容 | 整个 `Config`，含订阅链接和令牌；不含管理密码、缓存、访问记录 | 订阅组本身 + 它引用的来源；**去掉**订阅链接、解锁地址、手填节点、令牌、`Applied` |
| 导入 | 替换当前配置。令牌以文件为准（合法且不重复才保留），所以客户端里的链接不用改 | 追加为新订阅组：订阅组和来源都换新 ID、发新令牌，重名的名称后加「（导入）」 |

`POST /api/import` 按内容判断是哪种：有 `profile` 是单个订阅组；有 `config` 是整份配置（旧的 `config.json` 是 `{auth, config}`，也走这里，`auth` 被忽略）；都没有时试着把整个文件当 `Config`。之后一律是 `migrate` → `validateConfig` → 保存，任何一步失败都不改动现有配置。文件版本比 `configVersion` 新的直接拒绝。

单个订阅组的导出格式跟着数据模型走：来源是全局对象，所以要连带导出它引用的来源，导入时再给它们发新 ID 并改写引用。导入的来源不会和已有来源合并（链接已被去掉，无从判断是否相同）。

## 访问记录（`access.go`）

`handleSub` 每处理一次带有效令牌的拉取就记一条（包括返回 502 / 500 的），预览不记，令牌不对的请求不属于任何订阅组也不记。每个订阅组保留最近 100 条（`maxAccessRecords`），存在 `DATA_DIR/access.json`，写盘有 2 秒的合并延迟。订阅组被删除后（保存配置或导入时）它的记录一并清掉；重置令牌不清记录。

每条记录：时间、IP、UA、返回的节点数、状态码，以及 `NewIP`——这个 IP 此前有没有拉取过本订阅组（按订阅组记住最近 200 个 IP；第一个出现的 IP 不算新）。界面在「本组设置」里列出记录，最近 7 天有新 IP 时给一条提示。它只是帮人发现泄露的线索：手机换网络也会产生新 IP，所以不做任何自动处置。

**客户端 IP 怎么取（`clientIP`）**：`X-Forwarded-For` 是客户端可以随便填的，直接信任它，泄露链接的人就能伪造来源。规则是：

- 直接对端是公网地址 → 用对端地址，不看任何头
- 直接对端是回环 / 内网 / 链路本地地址 → 说明前面有反向代理（对端地址本身没有信息量），这时才看 `X-Forwarded-For`：从右往左跳过内网地址，取第一个公网地址（右边是离本服务近的代理追加的，左边可能是客户端伪造的）；全是内网地址时取最左边的；没有这个头再看 `X-Real-IP`

没有做成配置项。局限：反向代理必须**追加**而不是透传 `X-Forwarded-For`（Nginx 的 `$proxy_add_x_forwarded_for`、Caddy 默认行为都是）；前面还有一层公网 CDN 时，记到的是 CDN 的地址。这个 IP 只用于显示，不参与任何访问控制。

## 规则来源（`rulenames.go`）

规则来源（`Config.RuleSources`）是「第三方规则仓库」的描述，只服务于规则编辑器里的「从来源添加」：选一个来源、填一个规则名，前端的 `ruleSetFrom` 按来源的设置拼出一条 `RuleSet`（地址模板里的 `{name}` 换成规则名，名称加前缀，规则名匹配 `IPCIDR` 正则时类型改成 ipcidr）。**这是复制语义**，和模板一样：加进订阅组或模板的规则集和来源再无关系，`Build` 不读 `RuleSources`，改、删来源不影响已有的规则集。

一个来源分两部分：

- **清单从哪里列**：`Repo`（GitHub 的 owner/repo）、`Ref`（分支）、`Dir`（目录）、`Layout`（`file`：目录下每个文件一条规则，按 `Ext` 筛选并去掉扩展名；`dir`：每个子目录一条规则，blackmatrix7 是这种）。`Repo` 为空表示不拉清单
- **规则集怎么填**：`URL`（地址模板）、`Behavior`、`IPCIDR`、`Format`、`Prefix`

`Pinned` 是置顶的规则名，候选列表里排在最前面；没拉过清单时候选里只有它们。内置的五个来源（`seedRuleSources`）把各仓库的常用规则预先置顶，和内置模板一样只在新建配置或迁移时写入一次。

**规则清单**（仓库里有哪些规则名）不属于配置，存在 `DATA_DIR/rulenames.json`（来源 ID → 名字列表 + 拉取时间），不随导出。它**只在用户点「拉取」时更新**，没有定时任务，`Build` 也不触发。拉取是对 GitHub 接口的一次请求：`GET /repos/{repo}/git/trees/{ref}:{dir}`，列一层目录、不递归（用 git/trees 而不是 contents 接口，是因为后者一个目录最多返回 1000 条，MetaCubeX 的 geosite 目录有六千多个文件）。请求用默认的 `http.Client`，所以和上游订阅一样走环境变量里的代理。

- 匿名访问 GitHub 接口按出口 IP 每小时限 60 次，共用出口的代理很容易用完；设置环境变量 `GITHUB_TOKEN` 后带令牌访问。限流时返回的错误里会提示这一点
- 拉取失败不动已有的清单
- `POST /api/rule-names` 的请求体是来源本身而不是 ID：在界面上改了仓库、目录后不用先保存就能拉。清单按来源 ID 保存；保存时顺手清掉配置里已经不存在的来源留下的清单
- 服务端只会访问 `githubAPI`（测试里换成 httptest 的地址），`Repo` / `Ref` / `Dir` 在 `RuleSource.normalize` 里做了格式限制，不能借它访问别的地址
- 要支持 GitHub 以外的清单来源（GitLab、自建目录索引等），在 `fetchRuleNames` 里按来源分支即可，产出名字列表，其余不用动

## HTTP 接口

| 方法 路径 | 认证 | 说明 |
| --- | --- | --- |
| `GET /` | 否 | 管理页面 |
| `GET /api/status` | 否 | `{initialized, authed, envPassword}` |
| `POST /api/setup` | 否 | `{password}`，仅在尚未设置密码时可用 |
| `POST /api/login` | 否 | `{password}` |
| `POST /api/logout` | 否 | |
| `GET /api/config` | 是 | 返回整个 `Config` |
| `PUT /api/config` | 是 | 整体覆盖 `Config`（各订阅组的 `token` 会被忽略）；返回保存后的配置（含服务端分配的 ID 和令牌） |
| `POST /api/password` | 是 | `{old, new}` |
| `POST /api/token` | 是 | `{profile}`，重置该订阅组的令牌，返回新配置 |
| `GET /api/preview?profile=<id>&ua=<UA>[&refresh=1]` | 是 | 模拟该 UA 的客户端拉取；返回 `{yaml, subs, warnings, total, userinfo}`，`subs` 里有每个来源实际使用的链接和 UA |
| `GET /api/export[?profile=<id>]` | 是 | 下载整份配置；带 `profile` 时下载该订阅组的分享文件 |
| `POST /api/import` | 是 | 请求体是导出的文件内容；返回导入后的配置 |
| `GET /api/access?profile=<id>` | 是 | `{records}`，该订阅组的访问记录，最新的在前 |
| `DELETE /api/access?profile=<id>` | 是 | 清空该订阅组的访问记录 |
| `GET /api/rule-names` | 是 | `{来源 ID: {names, updated}}`，已拉取的全部规则清单 |
| `POST /api/rule-names` | 是 | 请求体是一个规则来源；从 GitHub 拉取它的规则清单并保存，返回 `{names, updated}`。拉取失败返回 502 |
| `GET /sub/{token}[?refresh=1]` | 令牌 | 该令牌对应订阅组的输出，以请求的 User-Agent 作为 clientUA，并记一条访问记录。响应头带 `Subscription-Userinfo`（各订阅流量求和、到期取最早）、`Profile-Update-Interval`、`Content-Disposition` |

错误统一返回 `{"error": "..."}`。前端每次保存都是把整个 `cfg` 对象 PUT 回去，没有局部更新接口；模板的新建、编辑、套用，来源的增删和引用，订阅组的新建、复制、删除都只是改这个对象。

## 前端结构（`web/index.html`）

没有框架：`h(tag, attrs, ...kids)` 建元素，`render()` 整页重绘。状态是几个模块级变量：`cfg`（整份配置）、`page`（`group` / `sources` / `templates` / `settings`）、`pi`（当前订阅组下标）、`tab`、`dirty`。

- **左栏 `rail()`**：列出所有订阅组，底部是「节点来源」「模板」「全局设置」「退出登录」
- **订阅组工作区 `groupPage()`**：标题是可直接改名的输入框；`GROUP_TABS` 定义标签页，每个对应一个 `tabXxx(p)` 函数，操作当前订阅组 `p`
- **`field(label, obj, key, opts)`**：绑定到 `obj[key]` 的表单控件，输入时写回并标记「未保存」。不属于配置的临时输入（预览 UA、对话框草稿）传 `local: true`
- **`item(obj, opts)`**：可展开的行（订阅源、策略组、模板都用它）。展开状态记在 `openItems`（WeakSet，按对象引用），所以重绘和排序后仍保持展开。`body(nameEl)` 只在展开时才构建；改名时直接更新 `nameEl` 而不重绘，避免输入框失焦
- **编辑器与页面分离**：策略组编辑器 `groupsEditor(o)`、规则编辑器 `rulesEditor(o)` 接收任何带 `groups` / `ruleSets` 等字段的对象，订阅组的标签页（`tabGroups` / `tabRules`）和「模板」页共用它们。规则里「策略」输入框的候选由 `policyNames(o)` 给出：订阅组有策略组可列；规则模板没有，只列已经用到的策略，其余自由输入。来源的表单 `sourceFields(s)` 只在「节点来源」页使用：订阅组的订阅源标签页（`tabSubs`）只能选用已有来源、设置本组的启用 / 前缀 / 过滤 / 前置代理，来源内容只读展示（`sourceSummary`），新建、修改、删除来源都要到「节点来源」页（`editSource` 跳转过去并展开）。写这类编辑器时不要用 `cur()`，同一页上可能同时展开好几个，`datalist` 的 id 用 `nextId()` 生成
- **`item` 可以嵌套**（模板行里展开策略组行）。CSS 里和展开 / 停用状态有关的选择器要用子代选择器（`.item.open > .item-head …`），否则外层的状态会套到内层上
- **`toolbar(hint, kind, p)`**：订阅组里「套用模板」按钮和差异提示。提示的刷新函数挂在模块级的 `liveHint` 上，`markDirty()` 时调用，`render()` 开头清掉
- **`dialog(title, build, actions)`**：基于原生 `<dialog>` 的对话框，套用模板、添加节点来源、新建订阅组都用它
- **`newID()`**：按服务端的格式生成 ID。新建来源和模板时用它，这样不用先保存就能被引用 / 套用；订阅组的 ID 仍然留空由服务端分配（它关系到令牌）
- **导出导入**：`download(path)` 用 `fetch` 取回再存成文件（不用直接跳转，以免触发「未保存」的离开提示）；`importFile()` 读文件后 POST 给 `/api/import`，整份配置导入前确认。访问记录按订阅组缓存在 `accessLog` 里，进入「本组设置」时加载
- **何时调用 `render()`**：结构变化（增删、排序、切换类型导致表单项变化）才重绘；普通文本输入不重绘，否则会丢焦点
- **`normalize(cfg)`**：从服务端拿到配置后补齐缺失字段。给数据模型加了数组、对象或字符串字段，在这里补默认值
- **配色**：集中在文件顶部的 CSS 变量（`:root` 与深色模式两组），改主题只改这里

<a id="cfw-parser"></a>
## CFW Parser

实现在 `parser.go`，语法依据 CFW 官方文档，并兼容 [Clash-Parser-Online](https://github.com/InoryS/Clash-Parser-Online) 的 `_` 操作符。

接受三种外层写法：直接写指令、`yaml:` 包裹、CFW 原版 `parsers: [{yaml: ...}]`（忽略 `url` / `reg`，所有条目依次执行）。**不支持** JavaScript 形式（`code` / `file` / `remote`）。

| 键 | 行为 |
| --- | --- |
| `prepend-<key>` / `append-<key>` | 数组合并到顶层 `<key>` 的前 / 后（`rules`、`proxies`、`proxy-groups` 或任意键） |
| `mix-<key>` | 对象浅合并到顶层 `<key>`（`proxy-providers`、`rule-providers` 等） |
| `mix-object` | 对象浅合并到最外层 |
| `commands` | 字符串数组；**无论写在哪，都在上面所有操作完成后**按顺序执行 |

### commands：`定位` + `操作` + `设定值`

**定位**：以 `.` 分隔。在数组里，纯数字是下标（从 0 开始），其他文本按元素的 `name`（或字符串元素本身）查找。名称含 `.`、`+`、`=` 时用括号包起来：`proxies.(a.b+c).udp`。

**操作**：

| 操作 | 含义 |
| --- | --- |
| `=` | 覆盖。定位到数组下标时替换该元素 |
| `+` | 插入。定位到数组下标 / 名称时插到该位置（原元素后移）；定位到数组本身时追加到末尾。设定值是数组时逐个展开 |
| 结尾的 `-` | 删除定位到的键或数组元素，如 `proxies.0-` |
| `_` | 按值删除（扩展）：`proxy-groups.(美国).proxies_节点名`、`rules_DOMAIN,a.com,DIRECT`。值为纯数字时按下标删 |

**设定值**：能按 JSON 解析就用解析结果（数字、`true`/`false`、对象、数组），否则当字符串。三个内置值：

- `[]proxyNames|正则`：当前所有节点名，按正则过滤（`|正则` 可省略）
- `[]groupNames|正则`：当前所有策略组名
- `[]shuffledProxyNames|正则`：同 proxyNames，但随机打乱

### 实现要点

- `parseCommand`：先在括号段之外找第一个 `=` 或 `+`；没有的话看是否以 `-` 结尾；再没有才找 `_`。所以键名里的 `-`（`proxy-groups`）和 `_` 不会被误判，但**设定值之前的定位里不能出现裸的 `=` / `+`**（要用括号）
- `applyPath`：递归下降，每层返回修改后的节点（因为数组插入 / 删除会产生新切片）。中间路径不存在且操作是 `=` / `+` 时自动创建对象
- 单条命令失败不会中断，会变成一条警告显示在预览里，其余命令继续执行
- 加新操作符：改 `parseCommand` 的识别 + `applyPath` 里 `len(segs)==0` 和数组 `last` 两处分支
- 加新内置值：改 `resolveValue`

## 常见扩展怎么做

**加一个 User-Agent 预设**：`web/index.html` 的 `UA_PRESETS` 加一行。后端不校验 UA，任意字符串都行。

**加一个规则源预设**：`web/index.html` 的 `RULE_SOURCES` 加一项（`url` 模板、`format`、`behavior`、`id` 前缀、`names` 候选）。预设只是帮用户填表，最终存的是普通 `RuleSet`，后端无需改动。

**加一套内置模板**：`store.go` 的 `seedTemplates` 加一项。注意它只对新安装和迁移生效，已有用户不会自动得到。

**让模板支持新的内容类型**（比如基础配置模板）：`Template` 加字段和新的 `Kind`，`validateConfig` 的 `switch t.Kind` 加分支，前端 `KINDS` 加一项（`fields` 写明要复制的字段，差异提示的摘要也按它算），在对应页面放一个 `toolbar(hint, kind, p)`，在 `templatesPage` 里加一个 `section` 和对应的编辑器。

**支持新的分享链接协议**：`uri.go` 的 `parseURI` 加一个 `case`，实现 `parseXxx(line) map[string]any`，返回 Clash 节点字段；在 `TestURIs` 里补用例。

**支持新的订阅格式**（如 sing-box JSON）：在 `fetch.go` 的 `parseSubscription` 里加一个分支，产出 `[]map[string]any`（Clash 节点格式）即可，下游不用动。

**加一种节点来源**（比如本地文件）：`Source.Kind` 加新值，`Build` 第 1 步的 goroutine 里加分支产出 `cacheEntry{Proxies: ...}`，`validateConfig` 里做对应校验，前端 `sourceFields` 加表单，`sourcesPage` 加「添加」按钮，`sourceSummary` 加只读概述；`exportProfile` 里把新类型里能拿到节点的字段也去掉。

**解锁步骤要更复杂**（POST、自定义 Header、多步）：改 `fetchSubscription`。给 `Source` 加影响拉取结果的字段后，记得把它加进 `cacheKey`，否则改了配置缓存不会失效。

**加一个内置规则来源**：`store.go` 的 `seedRuleSources` 加一项，同样只对新安装和迁移生效；已有用户在「规则来源」页自己添加。

**给策略组 / 来源 / 规则集加配置项**：① `store.go` 加结构体字段（来源的配置项先分清属于 `Source` 还是 `SourceRef`）；② `build.go` 使用它；③ `web/index.html` 对应的编辑器（`groupsEditor` / `rulesEditor` / `sourceFields` / `tabSubs`）里加一个 `field(...)`，以及「添加」按钮里新对象的默认值。模板会自动带上新字段（复制的是整个对象）。

**输出其他客户端格式**（Loon 等非 mihomo 系）：加一个输出侧的渲染器，接在 `Build` 第 6 步之后、替换第 7 步，把 Clash 结构的 `root` 转成目标格式；上游拉取和输入解析不用动。完整的事项清单（目标判定、透传 UA 的限制、基础配置、规则集链接、响应头）见 [CLIENT-SUPPORT.md](CLIENT-SUPPORT.md)。Stash 用的就是 Clash 格式，不需要渲染器。

## 开发与测试

```bash
go run .                 # http://localhost:8080，数据在 ./data
go test ./...            # 全部测试，不访问外网（用 httptest 模拟上游）
go vet ./... && gofmt -l .
```

改前端后重新 `go run .` 即可（HTML 是编译期嵌入的）。

改完先跑对应的测试：

- `TestUAPassthrough`：断言上游实际收到的「路径 + UA」及次数，以及失败时不跨 UA 回退。改 `resolveUA` 或缓存逻辑后跑它
- `TestBuildEndToEnd`：先解锁再拉取、解锁 UA（默认 Chrome UA，以及全局自定义后重新拉取）、过滤、手动节点、同一来源被引用两次（只拉一次上游）、停用的引用、重名处理、前瞻正则、Parser 引用手动节点、流量信息合并、输出可被重新解析、字符串类型不丢失、同一 UA 回退旧缓存
- `TestMigrateLegacyConfig`：v1、v2、v3 旧文件的迁移结果，以及迁移是幂等的
- `TestDialerProxy`：引用上的前置代理写成 `dialer-proxy`、不填时保留节点自带的值、目标不存在和成环的警告
- `TestMigrateSharedSources`：v4 → v5，相同的来源合并、过滤和前缀留在引用上、迁移结果能通过校验
- `TestValidateSourcesAndRefs`：来源 ID 的规范化、引用跟着改、悬空引用和失效的套用记录被去掉
- `TestFetchUsesProxy` / `TestProxySummary`：订阅请求和解锁请求都经过 `HTTP_PROXY`；日志里的代理说明不泄露密码
- `TestExportImport`：两种导出的内容（分享文件里没有链接和令牌）、单组导入是追加、整份导入保留令牌、旧版 `config.json` 能导入、坏文件不改动现有配置
- `TestAccessLog` / `TestClientIP`：记录的内容与顺序、新 IP 标记、落盘、上限、随订阅组删除；各种对端地址和转发头组合下取到的 IP
- `TestSeedTemplatesAreConsistent`：内置策略组模板和规则模板任意组合都没有悬空引用
- `TestRuleNames` / `TestValidateRuleSources`：两种目录结构下清单的筛选和排序、未保存的来源也能拉、各种失败不动已有清单、限流提示、`GITHUB_TOKEN`、清单落盘和清理；保存配置时规则来源的规范化和拒绝条件

## 已知限制

- `Dockerfile`（两阶段构建，`CGO_ENABLED=0`）编写时开发机没有 Docker，尚未实际构建验证过
- 管理界面没有随仓库的自动化测试。做来源共用、模板编辑、差异提示、导出导入、访问记录这一批功能时，用「临时数据目录的真实服务 + 无头 Edge（CDP）」把主流程实际点过一遍并断言了配置状态，但脚本没有收进仓库；其余交互只做过截图检查（浅色、深色、窄屏）
- 输出只有 Clash 格式。非 Clash 客户端（Shadowrocket 等）透传 UA 后上游返回的若是分享链接列表，能解析的协议见 `uri.go`，其余节点会被丢弃
- 在「模板」页编辑策略组模板时没有节点可参考（筛选正则写得对不对要套用到订阅组后预览才知道）；规则模板里的策略名是自由输入，不校验它和哪套策略组模板配套
- 差异提示的摘要忽略空值，「把某个字段从空改成 0 / false」不算修改
- 导入单个订阅组时带来的来源总是新建，不会和已有的相同来源合并
- 访问记录的 IP 依赖反向代理正确追加 `X-Forwarded-For`；前面有公网 CDN 时记到的是 CDN 地址
- 代理只能通过环境变量配置，所有来源共用，改了要重启服务
- 会话存内存，重启即失效；不支持多实例共享
- 规则集只是生成 `rule-providers` 让客户端自己下载，服务端不下载也不校验规则 URL 是否有效
- 规则清单只能从 GitHub 仓库拉，且只列一层目录；清单不会自动更新。规则来源里「规则名匹配此正则时按 ipcidr」用的是浏览器的正则，不走 `compileRegex`
- `text` / `mrs` 格式的规则集和部分节点类型需要 mihomo（Clash Meta）内核，原版 Clash 不支持
- `validate` 不解析逻辑规则（`AND` / `OR` / `NOT`）里的策略名
- 服务端会访问管理员填写的任意 http(s) 地址（订阅、解锁地址），没有内网地址限制；管理密码泄露等同于可从服务器发起请求
