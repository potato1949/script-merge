# 开发文档

面向后续维护和扩展功能。使用说明见 [README](../README.md)。

## 技术栈与约定

- Go 1.24+，单个 `package main`，无框架，路由用标准库 `net/http`（Go 1.22 的方法 + 路径模式）
- 依赖只有两个：`gopkg.in/yaml.v3`（YAML）、`github.com/dlclark/regexp2`（正则）
- 前端是单个 `web/index.html`（原生 JS，无构建步骤），用 `embed` 打进二进制
- 存储是 JSON 文件，没有数据库

**为什么用 regexp2 而不是标准库 `regexp`**：机场用户常用的节点筛选正则大量依赖前瞻断言（如 `^(?=.*香港)(?!.*IPLC).*$`），标准库的 RE2 不支持。所有面向用户的正则都必须走 `compileRegex` / `optRegex`（带 2 秒匹配超时，防止灾难性回溯）。

**术语**：「订阅组」(`Profile`) 是本服务的概念，一个订阅组对应一个输出链接；「策略组」是 Clash 的 proxy-group；「底板」是订阅组选定的一个机场，输出以它的完整配置为准。

## 设计取舍

这个项目经历过「自建策略组 + 第三方规则集 + 模板 + 规则仓库」和「沿用机场的策略组和规则」两套方案并存的阶段，结果两套逻辑互相叠加，用起来和维护起来都很乱。v8 重构时定为**只保留底板方案**（用户的选择）：

- 策略组和规则只来自底板机场，不提供策略组 / 规则编辑器、模板、规则集、规则仓库。要改就用 Parser
- 唯一的例外是订阅组可以**追加** select 策略组（`Profile.Groups`，见「新增的策略组」）：只追加在末尾，成员是选中的底板策略组加上正则匹配的节点；不改底板已有的组，也不写规则。删除底板的策略组讨论过，要处理规则和其他组里对它的引用，暂不做（见 ROADMAP）
- 底板的 `rule-providers` 原样输出，由客户端自己下载，服务端不平铺规则
- 没有全局的基础配置：端口、DNS 等都用底板的，要改用 Parser 的 `mix-object`
- 拉上游只用全局设置里的一个 UA（可以自动跟随 Clash Verge Rev 的最新版本），没有透传客户端 UA
- 不做历史版本、导出导入；旧结构的配置不迁移，直接作废

新功能如果又要引入「自己维护策略组 / 规则」，先回头看这里，别再把两套方案叠在一起。上一版的实现存档在 `archive/follow-mixed` 分支。

## 文件结构

| 文件 | 职责 |
| --- | --- |
| `main.go` | 启动、路由、认证与会话、HTTP 处理函数、保存配置时的校验（`validateConfig`） |
| `store.go` | 数据模型、JSON 持久化、旧配置作废、密码哈希、默认配置 |
| `fetch.go` | 拉取上游订阅（解锁、代理）、按链接 + UA 的缓存、订阅内容解析 |
| `build.go` | 核心流程 `Build`：底板 → 合并节点 → 策略组 → Parser → 校验 → 输出 YAML |
| `upstream.go` | 底板策略组的改写（`baseGroups`），其他来源节点要带上的 hosts（`nodeHosts`） |
| `groups.go` | 订阅组新增的策略组：生成（`addGroups`）和保存时的规范化（`normalizeGroups`） |
| `uri.go` | 分享链接（ss / vmess / trojan / vless / hysteria2）→ Clash 节点 |
| `parser.go` | CFW Parsers 语法实现、正则封装 |
| `access.go` | 订阅链接的访问记录、客户端 IP 的判定 |
| `version.go` | 程序版本：编译时指定的版本号，或构建信息里的 git tag / 提交 |
| `uafollow.go` | 拉取 UA 跟随 Clash Verge Rev 最新版本：定期查最新 release 的 tag |
| `web/index.html` | 管理界面 |
| `build_test.go` | 测试：端到端构建、底板不可用时报错、上游走代理、前置代理 |
| `api_test.go` | 测试：旧配置作废、配置校验（来源、引用、底板）、保存冲突、预览不保存与来源拉取状态、只用缓存的预览、来源缓存的查看 / 刷新 / 下载 |
| `upstream_test.go` | 测试：节点 hosts、上游配置的保存 |
| `groups_test.go` | 测试：新增的策略组（成员、顺序、跳过和警告、Parser 引用）、规范化 |
| `app_test.go` | 测试：Parser 命令语法与顺序、分享链接、代理说明 |
| `access_test.go` / `version_test.go` | 测试：访问记录与客户端 IP；程序版本 |

## 数据模型

```
Config
├─ Version / Revision      结构版本（见「存储」）/ 保存次数（见「保存冲突」）
├─ Sources[]               节点来源（全局，被订阅组引用）
│    ├─ ID / Name
│    ├─ Kind = remote：URL、UnlockURL
│    └─ Kind = manual：Nodes（YAML 或分享链接文本）
├─ Profiles[]              订阅组
│   ├─ ID / Name / Token / FileName
│   ├─ Base                底板机场的来源 ID
│   ├─ Sources[]           对节点来源的引用（SourceRef）：Source（来源 ID）、Enabled、Prefix、Include、Exclude、Dialer（前置代理）
│   ├─ Groups[]            新增的策略组（ProxyGroup）：ID、Name、Groups（底板策略组名）、Filter（节点名正则）、Icon（图标链接）
│   └─ Parser
└─ 全局：CacheMinutes、UpdateInterval、UserAgent（拉取 UA）、FollowUA（拉取 UA 跟随 Clash Verge Rev 版本）、UnlockUserAgent（解锁 UA）
```

**节点来源是全局对象，订阅组里只有引用。** 决定「从上游拿到什么」的属于来源本身（`Source`：链接、解锁地址、手填节点、名称）；决定「这个订阅组怎么用它」的属于引用（`SourceRef`：是否启用、前缀、Include / Exclude、前置代理）。同一个来源可以被多个订阅组引用，也可以在一个组里引用多次（比如按不同的正则拆成两批、加不同前缀）。缓存键里只有来源 ID，所以引用同一个来源的订阅组共用缓存。

**底板**（`Profile.Base`）必须是本组引用的远程来源。同一个来源在组里被引用多次时，第一条引用是底板，它的过滤和前缀生效，启用开关不看（底板始终使用）。`validateConfig` 保证这一点：底板不在本组里、或者是手动节点，就改成本组第一个远程来源，没有远程来源就留空（生成时报错）。前端的 `fixBase` 按同样的规则在增删引用时即时调整。

来源没有订阅组引用时仍然保留（「节点来源」页显示「未被使用」）。`validateConfig` 会去掉指向不存在来源的引用。

## 构建流程（`build.go`）

订阅链接 `GET /sub/{token}` 和预览 `POST /api/preview` 都调用 `Build(cfg, profile, fetcher, mode)`。区别在配置从哪来：订阅链接用已保存的配置；预览用请求体里带来的、前端当前的那份（可能还没保存），先过一遍 `validateConfig` 再构建，不落盘。「先预览、满意再保存」是界面的基本流程，不要让预览依赖已保存的配置。

`mode`（`FetchMode`）决定怎么用缓存：`FetchAuto` 是客户端拉取的流程（缓存过期才拉上游）；`FetchRefresh` 跳过缓存（`?refresh=1`、「节点来源」页的「刷新缓存」）；`FetchCacheOnly` 只用同一个键、当前格式的已有缓存（过期的也用），绝不请求上游。预览页没有常驻的生成按钮：每次进入预览页（`previewFor` 记着本次进入后已经生成过的订阅组，离开时清空），前端都按当前配置带 `cached: true` 重新生成一份，生成期间旧结果继续显示（`loadPreview`）；预览里的行按「订阅组 ID + 下标 + 来源 ID」（新增策略组按它的 ID）记展开状态，重新生成后照样展开。底板没有缓存时 `Build` 返回 `errNoCache`，接口回 `{noCache: true}`，界面显示「底板还没有缓存」和「拉取并生成」；其他来源没有缓存的只标 `SubStatus.NoCache`、不并入，界面提示并给一个「拉取」。这两个按钮按 `FetchAuto` 生成（`fetchPreview`），缺的、过期的缓存会去拉上游。缓存本身的信息（创建、过期时间）不在预览里，在「节点来源」页看（见「上游拉取与缓存」）。

```
1. 本组用到的来源：底板排第一个（不看启用开关），其余是启用的引用
2. 并发拉取       remote：Fetcher.Get（UA 见 Fetcher.userAgent：跟随版本时的 clash-verge/<版本>，自定义时全局的 UserAgent，空则 defaultUA）
                 manual：parseCustomProxies 解析 Nodes
   底板没拿到（拉取失败且没有缓存），或者内容里没有策略组 → 返回错误，不输出
3. root = 底板配置（去掉 proxies 后的整份，深拷贝）
4. 汇总节点       底板的在前，其余按引用顺序。按引用的 Include/Exclude 过滤 → 加 Prefix → 重名加序号
                 → 引用上有 Dialer 的写上 dialer-proxy。记下底板节点的「上游名 → 输出名」
                 非底板来源：顺着节点的 server 找出要用的上游 hosts / fake-ip-filter，合并进 root（nodeHosts）
5. 策略组         baseGroups：底板的策略组，成员里的节点名换成输出名，被过滤掉的去掉；
                 包含底板全部节点的组再追加其他来源的节点
                 addGroups：在末尾追加本组新增的策略组
6. ApplyParser    本组 Parser 对 root 做最终修改
7. validate       检查引用了不存在的策略 / 成员 / 前置代理、前置代理成环，只产生警告
8. toNode + 编码  输出键顺序稳定的 YAML
```

规则、`rule-providers`、`proxy-providers` 和其余设置都是底板原样的，`Build` 不碰。

**策略组的改写**（`upstream.go` 的 `baseGroups`）：

- 成员里是底板节点的，换成输出名（一个上游节点可能对应多个输出名：同一来源被引用多次时）；其他成员（策略组名、DIRECT、REJECT）原样保留
- 「包含底板全部节点」按底板上游的节点数判断（不看过滤），满足的组追加其他来源的节点。这样「节点选择」「自动选择」里能选到它们，按地区分的组不受影响
- 节点全被过滤掉、又没有 `use` / `include-all` / `filter` 的组填 `DIRECT` 并警告
- 用 `use`（proxy-providers）、`include-all`、`filter` 的组原样保留，由客户端处理
- 没有任何组包含其他来源的节点时（底板没有「全部节点」的组），给一条警告，提示用 Parser 加

**新增的策略组**（`groups.go` 的 `addGroups`）：

- 每项生成一个 `select` 组，追加在底板的策略组之后。成员：选中的底板策略组（按选择的顺序）在前，输出名（加了前缀、重名加了序号之后的）匹配 `Filter` 的节点（按输出顺序）在后。`Filter` 为空不加节点。`Icon` 非空时输出为组的 `icon` 字段（mihomo 系客户端显示），空则不输出
- 在 Parser 之前执行，所以 Parser 能引用新增的组。新增的组没有规则指向它，要让流量经过它得靠 Parser（加进底板的某个组、或写规则），这是有意的：不提供规则编辑
- 只能选底板的策略组（候选是 `SourceState.Groups`），不能选其他新增的组，也就不会成环
- 出错只警告不中断：选中的底板策略组已经不存在（机场改了名）跳过这个成员；组名和底板的策略组或节点重名，整组跳过；一个成员都没有填 `DIRECT`
- `BuildResult.NewGroups`（`GroupStatus`）按 `Profile.Groups` 的顺序记下每个组的成员或跳过的原因，按 `ID` 对应到界面上
- `normalizeGroups`（`validateConfig` 里调用）：ID 格式同来源 ID，不对或重复的重新生成（ID 只用于界面对应，不需要全局唯一）；组名和图标链接去掉首尾空白，组名不能为空、不能重复、不能是 `DIRECT` 这类内置策略，图标链接只接受 http(s)，否则拒绝保存；选中的底板策略组名去掉空的和重复的。是否和底板的组重名要拉到底板才知道，所以只在生成时检查

**前置代理（代理链）**：`SourceRef.Dialer` 非空时，这条引用下的节点都写上 `dialer-proxy`（值是策略组名或节点名），典型用法是「自建落地节点经由机场节点出去」。它属于引用而不是来源，不进缓存键；填了就覆盖节点自带的 `dialer-proxy`，没填则保留自带的。前置代理的成员里不能有以它为前置的节点，否则会绕回自己。`Build` **不会自动排除**，只由 `validateDialers` 报警告（这是用户确认的取舍）。这个检查在 Parser 之后做，所以手填节点自带的、Parser 写上的 `dialer-proxy` 也会被检查。

`BuildResult.Subs` 里每个用到的引用一条状态（`SubStatus`）：底板排第一（`Base` 为真），`Index` 是引用在订阅组 `Sources` 里的下标，`ID` 是来源 ID。同一个来源可能被引用多次，所以前端用「下标 + 来源 ID」对应到行上。`SubStatus.Nodes` 按上游顺序列出每个节点经过「只保留 / 排除」后的去向（`NodeChange`：原名，以及加了前缀、重名加了序号后的输出名，被过滤掉的没有输出名），预览页据此显示每个来源保留和过滤掉了哪些节点。流量信息（`Subscription-Userinfo`）按来源去重后求和，到期取最早。

内存中的配置树统一使用 `map[string]any` / `[]any`（不是 `[]map[string]any` 或 `[]string`），因为 Parser 的路径命令要对任意位置做通用遍历。往 `root` 里放数据时请保持这个约定，否则 `applyPath` 和 `toNode` 不认识。

### 节点地址解析用的 hosts（`nodeHosts`）

有的机场让节点的 `server` 经 `hosts` 映射到另一个域名（可能不止一层），再把这些域名放进 `dns.fake-ip-filter`。底板自己的 hosts 已经整份在输出里了；其他来源的只合并节点会丢掉这层映射，节点就连不上。所以 `Build` 从非底板来源实际用到的节点（过滤之后）的 `server` 出发，顺着它的上游 `hosts` 一层层找下去（键支持 `+.` / `.` / `*` 通配），把沿途用到的 hosts 条目，以及能匹配到这些域名的 fake-ip-filter 条目，合并进输出。和节点无关的不带。

- 自动进行，不需要配置；`SubStatus.Hosts` 记带过来的条数
- 底板里已有同名 hosts 的，以底板为准并警告
- fake-ip-filter 只在底板有 `dns` 段、且 `fake-ip-filter-mode` 不是 `whitelist` 时追加（白名单模式下追加会把意思弄反）

### 上游拉取与缓存（`fetch.go`）

- 每次拉取：先 GET `UnlockURL`（若有），再 GET 订阅地址。解锁只是一次普通的 GET——上游要求该地址被访问过之后才放行订阅——两次请求互相独立，不共享 Cookie（用户明确要求）。解锁请求用全局的解锁 UA `Config.UnlockUserAgent`（为空用 `chromeUA`），订阅请求的 UA 由 `Fetcher.userAgent` 决定，两种模式二选一：开了 `Config.FollowUA` 时用 `clash-verge/<最新版本>`，还没查到版本时用 `defaultUA`（不拿手填的 `UserAgent` 兜底：界面上选了跟随后输入框是隐藏的，用它就成了看不见的行为）；否则用 `Config.UserAgent`（为空用 `defaultUA`）。解锁请求只在网络失败时报错，不检查状态码
- 订阅内容解析（`parseSubscription`）：Clash YAML（有 `proxies`）→ base64 解码后的分享链接 → 明文分享链接。YAML 除了节点，其余内容整份存进 `upstream.Doc`，键统一转成字符串（`stringKeys`，否则 `map[any]any` 存不进 JSON 缓存）
- 缓存键 = `来源 ID + sha256(链接 + UA + UnlockURL)` 的前缀（`cacheKey`）；有解锁地址时解锁 UA 也计入。**给拉取加了影响结果的输入（不论在 `Source` 还是全局），必须加进 `cacheKey`**；`SourceRef` 上的字段只影响拉到之后怎么用，不进缓存键
- 内存一份 + `DATA_DIR/cache/<key>.json` 一份，启动时全部读入内存。缓存经过 JSON 往返后整数会变成 `float64`，`toNode` 会把整数值的 float 还原成整数输出
- 有效期是 `Config.CacheMinutes`（默认 5 分钟）。每个键有一把锁（`keyLock`），并发的相同请求只会拉一次上游。拉取失败后同一个键 30 秒内不重试（`failBackoff`）；`?refresh=1` 可绕过。每个来源最多保留 16 份缓存（`maxEntriesPerSub`），超出淘汰最旧的
- **失败时的回退**：`Fetcher.Get` 失败返回 `(同一个键的旧缓存或 nil, err)`，**不会**回退到其他链接 / UA 的结果（用户明确要求）。`Build` 据此设置 `SubStatus.Stale`
- 缓存内容有格式版本（`cacheFormat`）：改了缓存里存什么就加一，旧格式的缓存视为已过期（拉取失败时仍可作为旧缓存用）
- 来源 ID 被用作缓存文件名，所以 `validateConfig` 只接受 16 位十六进制且全局唯一的 ID，其他一律重新生成（防路径穿越）；来源 ID 变了，订阅组里的引用和底板跟着改
- **UA 跟随版本**（`uafollow.go`）：`Config.FollowUA` 开启时，`followUALoop` 每小时看一次是否到期，到期就查 `github.com/clash-verge-rev/clash-verge-rev/releases/latest` 的跳转地址（`…/releases/tag/vX.Y.Z`，只认数字版本号）。不用 `api.github.com`：未登录的接口按 IP 每小时限 60 次，共用出口 IP 时实测直接 403。成功后 30 天再查，失败后隔 1 天重试（版本号不用跟得很紧，用户要求一个月一次），失败时保留之前的版本。保存配置时如果开着跟随也会检查一次是否到期；界面上的「立即检查」不看间隔（`POST /api/ua-version`）。结果存在 `DATA_DIR/ua-version.json`，重启后沿用。界面上是「跟随 Clash Verge Rev 最新版 / 自定义」分段按钮（`uaField`），跟随时只显示当前版本、检查时间和「立即检查」，自定义时才显示输入框；切到跟随而还没有版本时立即查一次。版本变了 UA 就变了，缓存键随之改变，各来源下次会重新拉取
- `Fetcher.state` 记下每个来源最近一次**实际请求上游**的结果（`SourceState`：时间、错误、上次成功的时间 / 节点数 / 流量信息 / 策略组名），`GET /api/source-status` 给界面用：来源的状态标签、流量信息、前置代理的候选。只在内存里，重启后从磁盘缓存恢复成功的部分
- 「节点来源」页展开来源时顶部是信息面板（`sourceFacts`：缓存和使用它的订阅组），远程来源显示它的缓存（`POST /api/source-cache`）：节点数、创建时间、过期时间和剩余时间（按响应里服务端的 `now` 换算，不受浏览器时钟偏差影响），可以「刷新缓存」（跳过缓存重新拉取这一个来源）和「下载配置文件」（`POST /api/source-download`，把缓存还原成完整的 Clash 配置：`upstream.config()` 把节点放回 `proxies`，经 `encodeYAML` 输出，不是上游的原始文本）。两个接口和预览一样带着前端当前的配置，缓存键按其中的链接、UA 算（`fetchArgs`，和 `Build` 共用）；只校验全局设置和节点来源，订阅组的内容不影响。前端按 `cacheSig()`（链接、解锁地址、UA、跟随版本时的版本号、缓存时长）判断设置变了没有，变了或重新进入这个页面时重新读取；读取前先 `GET /api/ua-version` 同步版本（服务端可能已在后台换了 UA，缓存键随之改变）
- **代理**：只走环境变量 `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`。`fetchSubscription` 的 `http.Client` 没有自定义 `Transport`，落在 `http.DefaultTransport` 上，它本身就读这几个变量。**以后换了自定义 `Transport`，要带上 `Proxy: http.ProxyFromEnvironment`**，否则代理静默失效——`TestFetchUsesProxy` 会发现。标准库对回环地址永远不走代理，且环境变量只在第一次发请求时读一次，所以测试在 `TestMain` 里设置代理，并用一个不存在的域名当上游

上游内容只认 Clash YAML 或分享链接列表，输出始终是 Clash YAML。以后支持其他客户端的做法见 [CLIENT-SUPPORT.md](CLIENT-SUPPORT.md)。

### 输出 YAML（`toNode`）

- 顶层键顺序：`headKeys`（端口、模式等）→ 其余按字母序 → `tailKeys`（proxies、proxy-groups、proxy-providers、rule-providers、rules）
- 非顶层对象：`name, type, server, port` 在前，其余按字母序
- `proxies` 下每个节点输出成单行（flow style）
- 标量通过 `yaml.Node.Encode` 生成，所以 `"true"`、`"123"` 这类字符串会被正确加引号
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
- 订阅组的 `Token` 只由服务端生成：`PUT /api/config` 时按订阅组 ID 找回原令牌，前端传来的一律忽略；新订阅组分配新令牌。只能通过 `POST /api/token` 重置

**结构版本**：`Config.Version`，当前 `configVersion = 8`。**不做迁移**（用户明确说过旧数据不需要细致迁移）：`OpenStore` 读到版本不一致的文件，原文件存为 `config.json.bak`，配置换成默认配置（没有订阅组也没有来源；订阅组可以一个都没有，前端此时显示「节点来源」页），管理密码保留。所以：

- 只是**加字段**不需要改版本，但零值必须是合理的默认行为（或在 `validateConfig` / 使用处兜底），前端 `normalize()` 里补默认值
- **改结构**（改字段含义、删字段导致旧数据会出错）时 `configVersion` 加一，旧配置会被整个作废。动手前想清楚值不值得

**保存冲突**：前端每次保存都是把整个 `cfg` PUT 回去，两个页面同时开着时后保存的会盖掉先保存的。所以 `Config.Revision` 每保存一次加一（`Store.SetConfig`），`PUT /api/config` 带回的 `revision` 和当前的对不上就返回 409，前端询问用户后带 `?force=1` 重新提交。重置令牌这种服务端自己发起的保存不检查（`expect` 传 -1）。

`DATA_DIR/access.json`：访问记录。`DATA_DIR/cache/`：上游缓存。`DATA_DIR/ua-version.json`：跟随版本时查到的 Clash Verge Rev 版本。都不属于配置，删掉只是丢记录 / 重新拉取 / 重新查询。

## 访问记录（`access.go`）

`handleSub` 每处理一次带有效令牌的拉取就记一条（包括失败的），预览不记，令牌不对的请求不记。每个订阅组保留最近 100 条（`maxAccessRecords`），写盘有 2 秒的合并延迟。订阅组被删除后它的记录一并清掉；重置令牌不清记录。

每条记录：时间、IP、UA、返回的节点数、状态码，以及 `NewIP`——这个 IP 此前有没有拉取过本订阅组（按订阅组记住最近 200 个 IP；第一个出现的 IP 不算新）。它只是帮人发现泄露的线索，不做任何自动处置。

**客户端 IP 怎么取（`clientIP`）**：`X-Forwarded-For` 是客户端可以随便填的，直接信任它，泄露链接的人就能伪造来源。规则是：

- 直接对端是公网地址 → 用对端地址，不看任何头
- 直接对端是回环 / 内网 / 链路本地地址 → 说明前面有反向代理，这时才看 `X-Forwarded-For`：从右往左跳过内网地址，取第一个公网地址；全是内网地址时取最左边的；没有这个头再看 `X-Real-IP`

## HTTP 接口

| 方法 路径 | 认证 | 说明 |
| --- | --- | --- |
| `GET /` | 否 | 管理页面 |
| `GET /api/status` | 否 | `{initialized, authed, envPassword, build}`。`build` 是程序版本，只在已登录时返回 |
| `POST /api/setup` | 否 | `{password}`，仅在尚未设置密码时可用 |
| `POST /api/login` | 否 | `{password}` |
| `POST /api/logout` | 否 | |
| `GET /api/config` | 是 | 返回整个 `Config` |
| `PUT /api/config[?force=1]` | 是 | 整体覆盖 `Config`（各订阅组的 `token` 会被忽略）；返回保存后的配置。`revision` 和当前的对不上返回 409，带 `force=1` 不检查 |
| `POST /api/password` | 是 | `{old, new}` |
| `POST /api/token` | 是 | `{profile}`，重置该订阅组的令牌，返回新配置 |
| `GET /api/source-status` | 是 | `{来源 ID: SourceState}`，各来源最近一次实际请求上游的结果 |
| `GET /api/ua-version` | 是 | `UAVersion`：跟随版本时查到的 Clash Verge Rev 版本 `{version, okAt, checkedAt, error}`。`POST` 先立即查一次再返回 |
| `POST /api/source-cache` | 是 | `{config, source}`：按 `config`（前端当前的配置）里的设置返回各远程来源的缓存 `{caches: {来源 ID: CacheInfo}, now}`，没有缓存的不出现，不请求上游。带 `source` 时先跳过缓存重新拉取这个来源，失败的原因放在 `error` 里 |
| `POST /api/source-download` | 是 | `{config, source}` → `{yaml}`：这个来源的缓存还原成的完整配置。没有缓存返回 404，不请求上游 |
| `POST /api/preview` | 是 | `{config, profile, refresh}`：`config` 是前端当前的整份配置，`profile` 是订阅组在其中的下标。不保存。返回 `{yaml, subs, newGroups, warnings, total, groups, rules, userinfo}` |
| `POST /api/format-parser` | 是 | `{text}` → `{text}`：Parser 编辑框的「格式化」。`FormatParser` 经 `yaml.Node` 往返，统一 2 空格缩进，注释、引号、流式写法保留；YAML 有错返回 400 |
| `GET /api/access?profile=<id>` | 是 | `{records}`，该订阅组的访问记录，最新的在前 |
| `DELETE /api/access?profile=<id>` | 是 | 清空该订阅组的访问记录 |
| `GET /sub/{token}[?refresh=1]` | 令牌 | 该令牌对应订阅组的输出，并记一条访问记录。生成失败返回 500，没有节点返回 502（不返回空配置，免得客户端覆盖掉可用的旧配置）。响应头带 `Subscription-Userinfo`、`Profile-Update-Interval`、`Content-Disposition` |

错误统一返回 `{"error": "..."}`。

## 前端结构（`web/index.html`）

没有框架：`h(tag, attrs, ...kids)` 建元素（子元素数组可以任意嵌套，会被展平），`render()` 整页重绘。状态是几个模块级变量：`cfg`（整份配置）、`page`（`group` / `sources` / `settings`）、`pi`（当前订阅组下标）、`tab`、`saved`（最近一次从服务端拿到的配置）、`changes` / `dirty`（未保存的修改），以及不属于配置的 `srcStatus`（来源拉取状态）、`srcCache`（「节点来源」页显示的各来源缓存）、`previews`（各订阅组最近一次预览）、`accessLog`。

- **左栏 `rail()`**：列出所有订阅组（副标题是底板名），底部是「节点来源」「全局设置」「退出登录」
- **订阅组工作区 `groupPage()`**：标题是可直接改名的输入框；`GROUP_TABS` 定义标签页（节点来源、策略组、Parser、预览、本组设置），每个对应一个 `tabXxx(p)`
- **节点来源标签页 `tabSubs(p)`**：底板那行带「底板」标签、启用框禁用；其他远程来源有「设为底板」。只能选用已有来源、移除引用、改 `SourceRef` 上的字段；来源内容只读（`sourceSummary`），新建、修改和删除只在「节点来源」页（`sourcesPage`），免得在一个组里顺手一改、别的组跟着变。订阅组里不放新建来源或跳转到「节点来源」页的入口。增删引用后调 `fixBase(p)`
- **策略组标签页 `tabGroups(p)`**：本组新增的策略组，每项可展开，填名称、节点正则、图标链接（行首显示图标预览，`groupIcon`：没有或加载失败时隐藏但占位，各行名称对齐），勾选底板策略组（`groupPicker`：候选取自 `srcStatus[p.base].groups`，之前选过、底板里已经没有的也列出来以便取消；勾选的顺序就是成员顺序，标上序号；勾选时只更新序号和修改标记，不重绘）。生成过预览就在行上显示成员数或跳过的原因，预览页也列出每个新增组的成员
- **`field(label, obj, key, opts)`**：绑定到 `obj[key]` 的表单控件，输入时写回并调 `markDirty()`。不属于配置的临时输入传 `local: true`
- **未保存的修改**：加载、保存成功、重置令牌后把配置存一份到 `saved`。`markDirty()` 用 `diffConfig(cfg, saved)` 逐项对比得出 `changes`（改回原值的不算），每项带位置（`page` / `pid` / `tab`）、对象和字段（`obj` / `key`）以及清单上显示的文字。引用没有 ID，按「同一来源的第 n 次引用」配对；新增的策略组按 ID 配对。页面上的标记（左栏、标签页、行上的圆点 `dot()`，输入框的竖条 `track()`）在渲染时用 `watch()` 登记。标记一律画在控件内侧（`inset` 阴影），不向外凸出，免得控件看起来和同一列错开；来源行里的启用复选框靠行上的圆点标出，不单独加标记；新增策略组的那组勾选框整体当一个控件（`.checks`，外框和输入框一样），用竖条标记。被隐藏的字段要把标记和跳转挂到可见的控件上（如跟随版本时隐藏的自定义 UA 挂在分段按钮上），`markDirty()` 时逐个更新，不重绘。清单里点一项调 `jump()`：切页面和标签页、展开行、滚动并聚焦到输入框。「放弃修改」把 `cfg` 换回 `saved` 的副本，并丢掉用未保存内容生成的预览。给配置加了用户可改的字段，要加进 `diffConfig` 对应的 `*_FIELDS`，否则改了它不会被当成修改
- **`item(obj, opts)`**：可展开的行。`body(nameEl)` 只在展开时构建。展开状态按稳定的键记在 `openKeys`（`setOpen` 写回 localStorage 的 `script-merge.open`，最多留 500 个）：用户要求展开之后一直开着，保存、刷新、切换页面都不收起，直到手动收起。键由 `rowKey(obj)` 算：来源 `src:ID`，订阅组里的引用没有 ID，用 `ref:订阅组 ID:来源 ID:同一来源的第 n 次引用`，新增策略组 `grp:订阅组 ID:ID`；不属于配置的行（预览）由调用方传 `opts.key`。不要改回按对象引用记：保存后 `cfg` 换成服务端返回的新对象，按引用记会全部收起。各列表旁的「全部展开 / 全部收起」是 `foldAll(keys)`（有收起的就全部展开，否则全部收起）
- **`dialog(title, build, actions)`**：基于原生 `<dialog>` 的对话框
- **复制 `copyText(text, done)`**：`navigator.clipboard` 只在 HTTPS 或 localhost 下才有，用 `http://内网地址` 访问时没有它，所以退回 `execCommand('copy')`，再不行就弹出对话框（`manualCopy`）把文字选中让用户自己复制。不要直接调 `navigator.clipboard`
- **`newID()`**：按服务端的格式生成 ID。新建来源和订阅组时用它，这样不用先保存就能被引用、预览；令牌始终由服务端发，判断「还没保存过」看 `p.token`
- **保存 `save()`**：409 时询问是否覆盖。**预览**把 `cfg` 整份 POST 过去，不调 `save()`。订阅组标题旁的「下载配置文件」（`downloadProfile`）例外：它要和订阅链接的输出一致，所以把 `saved` 和订阅组在其中的下标交给 `POST /api/preview`（不带 `cached`，按客户端拉取的缓存规则），不记访问记录；有未保存的修改时提示下载的不含这些修改
- **何时调用 `render()`**：结构变化（增删、排序、换底板）才重绘；普通文本输入不重绘，否则会丢焦点
- **`normalize(cfg)`**：从服务端拿到配置后补齐缺失字段。给数据模型加了字段，在这里补默认值
- **配色**：集中在文件顶部的 CSS 变量（浅色、深色两组），风格参考 Claude 官网

界面没有随仓库的自动化测试。改界面后用无头浏览器实际检查：起一个临时 `DATA_DIR` 的真实服务和一个返回固定内容的假上游，用 `msedge --headless=new --remote-debugging-port=…` 通过 CDP 执行点击并断言 `cfg` 的状态（顶层的 `let` / `const` 在 `Runtime.evaluate` 里可以直接访问），再截图看一眼。

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

**支持新的分享链接协议**：`uri.go` 的 `parseURI` 加一个 `case`，实现 `parseXxx(line) map[string]any`，返回 Clash 节点字段；在 `TestURIs` 里补用例。

**支持新的订阅格式**（如 sing-box JSON）：在 `fetch.go` 的 `parseSubscription` 里加一个分支，产出节点即可。这种来源不能做底板（没有策略组）。

**加一种节点来源**（比如本地文件）：`Source.Kind` 加新值，`Build` 第 2 步的 goroutine 里加分支产出 `cacheEntry{upstream: upstream{Proxies: ...}}`，`validateConfig` 里做对应校验，前端 `sourceFields` 加表单，`sourcesPage` 加「添加」按钮，`sourceSummary` 加只读概述。

**解锁步骤要更复杂**（POST、自定义 Header、多步）：改 `fetchSubscription`。给 `Source` 加影响拉取结果的字段后，记得把它加进 `cacheKey`。

**给来源引用加配置项**：① `store.go` 的 `SourceRef` 加字段（先确认它属于引用而不是来源）；② `build.go` 使用它；③ `web/index.html` 的 `tabSubs` 里加一个 `field(...)`，`blankRef` 和 `normalize` 里补默认值。

**输出其他客户端格式**（Loon 等非 mihomo 系）：加一个输出侧的渲染器，接在 `validate` 之后、替换 YAML 编码；上游拉取和输入解析不用动。见 [CLIENT-SUPPORT.md](CLIENT-SUPPORT.md)。

## 开发与测试

```bash
go run .                 # http://localhost:8080，数据在 ./data
go test ./...            # 全部测试，不访问外网（用 httptest 模拟上游）
go test -count=20 ./...  # 改了并发 / 缓存相关代码后多跑几轮
go vet ./... && gofmt -l .
```

改前端后重新 `go run .` 即可（HTML 是编译期嵌入的）。

- `TestBuildEndToEnd`：底板排第一且不看启用开关、先解锁再拉取、解锁 UA、过滤、手动节点、同一来源引用两次（只拉一次上游、流量只算一次）、停用的引用、重名处理、底板策略组的改写（全部节点的组带上其他节点、部分节点的组只换名、全被过滤的填 DIRECT、`use` 原样保留）、底板的规则和其他设置原样保留、其他来源的 hosts、Parser、失败时回退到磁盘缓存且结果不变
- `TestBuildBaseErrors`：没选底板、底板是手动节点、底板不在本组、底板没有策略组、底板拉取失败
- `TestDialerProxy`：前置代理写成 `dialer-proxy`、不填时保留节点自带的值、目标不存在和成环的警告
- `TestFetchUsesProxy` / `TestProxySummary`：订阅请求和解锁请求都经过 `HTTP_PROXY`；日志里的代理说明不泄露密码
- `TestAddGroups` / `TestNormalizeGroups`：新增策略组的成员和顺序、和底板策略组或节点重名时跳过、底板策略组不存在、正则写错、没有成员填 DIRECT、Parser 能引用；ID、组名、底板策略组名的规范化和拒绝保存的情况
- `TestValidateSourcesAndRefs`：来源 ID 的规范化、引用跟着改、悬空引用被去掉、底板的修正
- `TestOldConfigReset`：旧版本配置作废、留下备份、密码保留
- `TestSaveConflict` / `TestPreviewUnsavedAndSourceStatus`：拿着旧版本号保存被拒绝、`force` 能覆盖；预览不落盘；来源状态在成功、失败、重启后的取值
- `TestPreviewCachedOnly`：只用缓存的预览不请求上游；底板没缓存返回 `noCache`；其他来源没缓存只做标记；过期的缓存照用
- `TestSourceCacheAPI`：来源缓存按请求里的设置查找、不请求上游；刷新跳过缓存、失败时带原因并保留旧缓存；换 UA 后是另一份缓存；下载还原出完整配置；Parser 写错不影响
- `TestUAFollow` / `TestUAVersionDue` / `TestUAFollowAPI`：跟随版本时的 UA、还没查到版本时用 defaultUA、版本落盘、查询失败或版本号无法识别时保留旧版本、检查间隔；预览拉取上游用的是查到的版本
- `TestNodeHosts` / `TestParseSubscriptionKeepsDoc`：hosts 的查找与合并；上游配置去掉节点后整份留下、非字符串的键能存进缓存
- `TestAccessLog` / `TestClientIP` / `TestBuildInfo` / `TestTagVersion` / `TestCommands` / `TestParserOrder` / `TestURIs`：见各自的注释

## 版本号

程序版本是 `主.次.修订`（`vX.Y.Z`），从 v1.0.0 开始，取自 git tag，代码里不写死。发布的约定是在 `main` 上打带注释的 tag；程序不检查 tag 类型和所在分支，所以同一个提交只打一个发布 tag，也不要打临时的 `v*` tag。

| 加哪一位 | 什么时候 | 例子 |
| --- | --- | --- |
| 主版本 X | 升级后用户要手动处理的不兼容变化 | `configVersion` 加一（旧配置作废）；订阅链接格式变了、客户端里的旧链接失效；去掉已有功能、环境变量或接口；Parser 语法不再兼容 |
| 次版本 Y | 向下兼容的新功能或行为变化 | 新的来源类型、输出格式；新配置字段（零值等于原来的行为）；界面上的新功能 |
| 修订 Z | 不加功能的修正 | bug 修复、界面文案和样式微调、性能、依赖的小版本升级 |

- 加主版本 / 次版本时后面的位归零；一次发布里有多种变化时按最高的那一档算
- 只改文档、测试、开发工具（构建出的程序不变）不发新版本
- `cacheFormat` 加一不算不兼容（旧缓存自动视为过期），按同一次改动的其他内容定
- 不是每个提交都打 tag，攒到要部署 / 发布时再打。打之前在 `main` 上跑完 `go vet`、`gofmt -l`、`go test`
- tag 说明写这一版的变化摘要（中文），它就是更新记录
- **加主版本时先改 `go.mod`**：模块路径改成 `module script-merge/vN`（v2 起；单个 `package main`，没有内部导入路径，只改这一行），和这一版的改动一起提交，再在这个提交上打 tag。不改的话 Go 按模块版本规则会忽略 v2 及以上的 tag，`go build` 出的程序显示提交号，同一提交上还有 v1 的 tag 时甚至显示成旧版本。单元测试发现不了（tag 在写进构建信息前就被过滤了），打完 tag 后 `go build` 一次，用 `go version -m script-merge` 确认 `mod` 一行的版本就是新 tag

```bash
git tag -a v1.1.0 -m "v1.1.0：……"
git push origin v1.1.0
```

程序里显示的版本（`version.go`）：`-ldflags "-X main.version=…"` 指定的优先；否则当前提交正好在 tag 上时，用 `go build` 自动写进构建信息的 tag（Go 1.24 起）；再否则显示提交号前 7 位。Docker 构建拿不到 `.git`，由 `docker-compose.yml` 把环境变量 `SCRIPT_MERGE_VERSION` 作为构建参数 `VERSION` 传进去（外面的变量名带项目前缀，免得误用 shell 或 `.env` 里别的工具的 `VERSION`），用 `git describe --tags --always --dirty`，tag 之后还有提交时显示成 `v1.1.0-3-gabc1234`；PowerShell 的写法见 README。这条路径只有版本字符串，没有提交号和时间。

- 指定的版本号末尾的 `-dirty` 当作状态标记：去掉后标记为「含未提交的修改」，和 `go build` 的显示一致。所以预发布名不要用 `-dirty`
- 两条路径对「未提交的修改」的判断不完全相同：`git describe --dirty` 不算未跟踪的文件，`go build` 算
- 两条路径选 tag 的规则也不同：`go build` 按 Go 模块版本规则（只认语义版本、主版本要和模块路径对上），`git describe --tags` 认任何 tag，同一提交有多个 tag 时挑法也不一样。按上面的约定打 tag 就不会有差别

## 已知限制

- `Dockerfile`（两阶段构建，`CGO_ENABLED=0`）尚未实际构建验证过
- 输出只有 Clash 格式
- 底板必须返回带策略组的 Clash 配置；机场按 UA 返回内容，全局只有一个拉取 UA，不同机场需要不同 UA 时无法分别设置
- 底板的 `rule-providers` 由客户端自己下载，国内网络下 GitHub 上的规则文件可能下载失败
- 其他来源的节点只会加进「包含底板全部节点」的组；底板没有这种组时要靠 Parser
- 来源的拉取状态只在内存里：重启后失败记录丢失，只能从磁盘缓存恢复「上次成功」
- 预览未保存的新来源也会写上游缓存，之后没保存的话缓存文件留在磁盘上
- 访问记录的 IP 依赖反向代理正确追加 `X-Forwarded-For`；前面有公网 CDN 时记到的是 CDN 地址
- 代理只能通过环境变量配置，所有来源共用，改了要重启服务
- 会话存内存，重启即失效；不支持多实例共享
- `validate` 不解析逻辑规则（`AND` / `OR` / `NOT`）和 `SUB-RULE` 里的策略名
- 服务端会访问管理员填写的任意 http(s) 地址（订阅、解锁地址），没有内网地址限制；管理密码泄露等同于可从服务器发起请求
