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
| `store.go` | 数据模型、JSON 持久化、旧配置迁移、密码哈希、默认配置与内置模板 |
| `fetch.go` | 拉取上游订阅（解锁）、按链接 + UA 的缓存、订阅内容解析入口 |
| `uri.go` | 分享链接（ss / vmess / trojan / vless / hysteria2）→ Clash 节点 |
| `build.go` | 核心流程 `Build`：决定拉取用的 UA → 合并节点 → 策略组 → 规则 → Parser → 校验 → 输出 YAML |
| `parser.go` | CFW Parsers 语法实现、正则封装 |
| `web/index.html` | 管理界面，以及 UA 预设、规则源预设 |
| `app_test.go` | 测试：命令语法、Parser 顺序、分享链接、端到端构建、UA 透传、迁移、内置模板一致性 |

## 数据模型

```
Config
├─ Version                 配置结构版本，见「迁移」
├─ Profiles[]              订阅组，彼此完全独立
│   ├─ ID / Name / Token / FileName
│   ├─ Subscriptions[]     节点来源
│   │    ├─ Kind = remote：URL、UAMode、UnlockURL
│   │    ├─ Kind = manual：Nodes（YAML 或分享链接文本）
│   │    └─ 两种都有：Enabled、Prefix、Include、Exclude
│   ├─ Groups[]            策略组
│   ├─ Rules               RulesBefore / RuleSets[] / RulesAfter（内嵌结构体）
│   └─ Parser
├─ Templates[]             模板：Kind = groups / rules / parser，带对应那部分内容
└─ 全局：BaseConfig、CacheMinutes、UpdateInterval、UserAgent（固定 UA）、UnlockUserAgent（解锁 UA）
```

**模板是复制语义**：套用模板 = 把模板内容复制进订阅组，之后两者互不影响。所以服务端只负责保存 `Templates`，「套用」「存为模板」都是前端对 `cfg` 对象的操作（`KINDS[kind].fields` 列出了互相复制的字段），`Build` 完全不知道模板的存在。内置模板（`seedTemplates`）只在新建配置或迁移时写入一次，之后和用户自建的没有区别，可以改名、删除。

内置的两套策略组模板和两套规则模板共用同一批策略组名（`gProxy` 等常量），所以可以任意组合；`TestSeedTemplatesAreConsistent` 保证这一点。新增内置模板时沿用这些名字。

## 构建流程

订阅链接 `GET /sub/{token}`（按 token 找到订阅组）和预览 `GET /api/preview` 都调用同一个 `Build(cfg, profile, fetcher, clientUA, force)`。`clientUA` 是来拉合并链接的客户端的 UA（预览时由 `?ua=` 指定）：

```
基础配置 (cfg.BaseConfig) → root map
  1. 并发处理本组所有启用的节点来源
       remote：resolveUA 决定 UA → Fetcher.Get → parseSubscription
       manual：parseCustomProxies 解析 Nodes
  2. 汇总节点        Include/Exclude 过滤 → 加 Prefix → 重名加序号
  3. 生成策略组      Extra（固定成员）在前，按 Nodes 模式追加节点
  4. 生成规则        RulesBefore → 每个 RuleSet 一条 RULE-SET → RulesAfter
  5. ApplyParser     本组 Parser 对 root 做最终修改
  6. validate        检查引用了不存在的策略 / 成员，只产生警告
  7. toNode + 编码   输出键顺序稳定的 YAML
```

手动节点在第 1 步就进入节点列表，所以会参与策略组筛选，Parser 也能引用它们。

内存中的配置树统一使用 `map[string]any` / `[]any`（不是 `[]map[string]any` 或 `[]string`），因为 Parser 的路径命令要对任意位置做通用遍历。往 `root` 里放数据时请保持这个约定，否则 `applyPath` 和 `toNode` 不认识。

### 拉取上游用的 UA（`resolveUA`）

- **链接**：每个订阅只有一个 `URL`。为空时该来源不提供节点（状态里报错，不影响其他来源）
- **UA**：`UAMode == "passthrough"` 且客户端带了 UA 时用 `clientUA`；否则用全局的固定 UA `Config.UserAgent`；再为空用 `defaultUA`。订阅自己不带 UA，只选模式
- `clientUA` 在 `main.go` 里被截断到 256 字符

上游内容只认 Clash YAML 或分享链接列表，不按客户端去拉它的原生格式；输出格式也不随客户端变化，始终是 Clash YAML。这样设计的理由和以后支持其他客户端的做法见 [CLIENT-SUPPORT.md](CLIENT-SUPPORT.md)。

### 上游拉取与缓存（`fetch.go`）

- 每次拉取：先 GET `UnlockURL`（若有），再 GET 订阅地址。解锁只是一次普通的 GET——上游要求该地址被访问过之后才放行订阅——两次请求互相独立，不共享 Cookie。解锁请求使用全局的解锁 UA `Config.UnlockUserAgent`（为空用 `chromeUA` 常量，即 Chrome 的 UA），不随订阅的 UA 模式变化；订阅请求使用 `resolveUA` 给出的 UA。解锁请求只在网络失败时报错，不检查状态码
- 缓存键 = `订阅 ID + sha256(实际链接 + 实际 UA + UnlockURL)` 的前缀（`cacheKey`）；有解锁地址时解锁 UA 也计入。同一订阅被不同 UA 透传拉取会各存一份；固定 UA 的订阅不管哪个客户端来都共用一份。内存一份 + `DATA_DIR/cache/<key>.json` 一份，启动时全部读入内存
- 有效期是 `Config.CacheMinutes`（默认 5 分钟）。有效期内同一个键不会再请求上游
- 每个键有一把锁（`keyLock`），并发的相同请求只会拉一次上游
- 拉取失败后同一个键 30 秒内不重试（`failBackoff`），避免上游故障时每次客户端请求都去打上游；`?refresh=1` 可绕过
- 每个订阅最多保留 16 份缓存（`maxEntriesPerSub`），超出淘汰最旧的。透传模式下缓存份数由客户端 UA 的种类决定，这个上限防止无限增长
- **失败时的回退**：`Fetcher.Get` 成功返回 `(entry, nil)`；失败返回 `(同一个键的旧缓存或 nil, err)`。**不会**回退到同一订阅其他链接 / UA 的结果——不同 UA 拿到的节点可能不同，某个 UA 失败就是失败。`Build` 据此设置 `SubStatus.Stale`
- 修改订阅的链接、UA 模式、解锁地址，或全局的固定 UA、解锁 UA 后，受影响订阅的缓存键随之改变，旧缓存不会被命中（留在磁盘上直到被淘汰）
- 缓存经过 JSON 往返后整数会变成 `float64`，`toNode` 会把整数值的 float 还原成整数输出
- 订阅 ID 被用作缓存文件名，所以 `validateConfig` 只接受 16 位十六进制且全局唯一的 ID，其他一律由服务端重新生成（防路径穿越）
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
- 订阅组的 `Token` 只由服务端生成：`PUT /api/config` 时按订阅组 ID 找回原令牌，前端传来的 `token` 一律忽略；新订阅组（ID 未知或为空）分配新 ID 和新令牌

### 迁移

`Config.Version` 记录结构版本，`Config.migrate` 在 `OpenStore` 时把旧文件升到 `configVersion`，迁移前的原文件保存为 `config.json.bak`。

| 版本 | 结构 | 迁移到下一版时 |
| --- | --- | --- |
| v1（无 `version`，无 `profiles`） | 只有一组订阅，字段都在顶层 | 顶层的 token / fileName / subscriptions / customProxies 装进名为「默认」的订阅组，原订阅链接不变 |
| v2（无 `version`，有 `profiles`） | 多订阅组，但策略组 / 规则 / 通用 Parser 在顶层共用 | 共用的策略组和规则复制进每个订阅组；`customProxies` 变成一个 manual 来源；通用 Parser 存为模板，并填入 Parser 为空的订阅组（两边都有内容时无法自动合并，只保留为模板）；写入内置模板 |
| v3 | 策略组、规则、Parser 都在订阅组内；订阅带「专用链接」（`variants`，按客户端 UA 切换上游链接）和各自的固定 UA（`userAgent`） | 移除专用链接：默认链接为空的订阅改用第一条有链接的专用链接，其余专用链接丢弃。固定 UA 挪到全局：取第一个填了 UA 的订阅的值（与内置默认值相同则留空），其余丢弃 |
| v4 | 当前结构 | |

旧字段以 `Legacy*` 命名、`omitempty` 保留在结构体里，只用于读取旧文件。以后改结构：`configVersion` 加一，在 `migrate` 里加一段，并在 `TestMigrateLegacyConfig` 补一个旧文件样例。

只是**加字段**不需要迁移：旧文件里没有这个字段，读出来是零值，所以零值必须是合理的默认行为（或在 `validateConfig` / 使用处兜底）。例如 `UAMode` 为空等同于 `fixed`，`Kind` 为空等同于 `remote`。

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
| `GET /sub/{token}[?refresh=1]` | 令牌 | 该令牌对应订阅组的输出，以请求的 User-Agent 作为 clientUA。响应头带 `Subscription-Userinfo`（各订阅流量求和、到期取最早）、`Profile-Update-Interval`、`Content-Disposition` |

错误统一返回 `{"error": "..."}`。前端每次保存都是把整个 `cfg` 对象 PUT 回去，没有局部更新接口；模板的套用、另存、订阅组的新建、复制、删除都只是改这个对象。

## 前端结构（`web/index.html`）

没有框架：`h(tag, attrs, ...kids)` 建元素，`render()` 整页重绘。状态是几个模块级变量：`cfg`（整份配置）、`page`（`group` / `templates` / `settings`）、`pi`（当前订阅组下标）、`tab`、`dirty`。

- **左栏 `rail()`**：列出所有订阅组，底部是「模板」「全局设置」「退出登录」
- **订阅组工作区 `groupPage()`**：标题是可直接改名的输入框；`GROUP_TABS` 定义标签页，每个对应一个 `tabXxx(p)` 函数，操作当前订阅组 `p`
- **`field(label, obj, key, opts)`**：绑定到 `obj[key]` 的表单控件，输入时写回并标记「未保存」。不属于配置的临时输入（预览 UA、对话框草稿）传 `local: true`
- **`item(obj, opts)`**：可展开的行（订阅源、策略组、模板都用它）。展开状态记在 `openItems`（WeakSet，按对象引用），所以重绘和排序后仍保持展开。`body(nameEl)` 只在展开时才构建；改名时直接更新 `nameEl` 而不重绘，避免输入框失焦
- **`dialog(title, build, actions)`**：基于原生 `<dialog>` 的对话框，套用模板、存为模板、新建订阅组都用它
- **何时调用 `render()`**：结构变化（增删、排序、切换类型导致表单项变化）才重绘；普通文本输入不重绘，否则会丢焦点
- **`normalize(cfg)`**：从服务端拿到配置后补齐缺失字段。给数据模型加了数组或字符串字段，在这里补默认值
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

**让模板支持新的内容类型**（比如基础配置模板）：`Template` 加字段和新的 `Kind`，`validateConfig` 的 `switch t.Kind` 加分支，前端 `KINDS` 加一项（`fields` 写明要复制的字段），在对应页面放一个 `toolbar(hint, kind)`。

**支持新的分享链接协议**：`uri.go` 的 `parseURI` 加一个 `case`，实现 `parseXxx(line) map[string]any`，返回 Clash 节点字段；在 `TestURIs` 里补用例。

**支持新的订阅格式**（如 sing-box JSON）：在 `fetch.go` 的 `parseSubscription` 里加一个分支，产出 `[]map[string]any`（Clash 节点格式）即可，下游不用动。

**加一种节点来源**（比如本地文件）：`Subscription.Kind` 加新值，`Build` 第 1 步的 goroutine 里加分支产出 `cacheEntry{Proxies: ...}`，`validateConfig` 里做对应校验，前端 `tabSubs` 加表单和「添加」按钮。

**解锁步骤要更复杂**（POST、自定义 Header、多步）：改 `fetchSubscription`。给 `Subscription` 加影响拉取结果的字段后，记得把它加进 `cacheKey`，否则改了配置缓存不会失效。

**给策略组 / 订阅 / 规则集加配置项**：① `store.go` 加结构体字段；② `build.go` 使用它；③ `web/index.html` 对应的 `tabXxx` 里加一个 `field(...)`，以及「添加」按钮里新对象的默认值。模板会自动带上新字段（复制的是整个对象）。

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
- `TestBuildEndToEnd`：先解锁再拉取、解锁 UA（默认 Chrome UA，以及全局自定义后重新拉取）、过滤、手动节点、重名处理、前瞻正则、Parser 引用手动节点、流量信息合并、输出可被重新解析、字符串类型不丢失、同一 UA 回退旧缓存
- `TestMigrateLegacyConfig`：v1、v2、v3 旧文件的迁移结果，以及迁移是幂等的
- `TestSeedTemplatesAreConsistent`：内置策略组模板和规则模板任意组合都没有悬空引用

## 已知限制

- `Dockerfile`（两阶段构建，`CGO_ENABLED=0`）编写时开发机没有 Docker，尚未实际构建验证过
- 管理界面没有自动化测试。只用无头浏览器 + 模拟数据截图检查过各页面和对话框的渲染（浅色、深色、窄屏），按钮交互未逐项点过
- 输出只有 Clash 格式。非 Clash 客户端（Shadowrocket 等）透传 UA 后上游返回的若是分享链接列表，能解析的协议见 `uri.go`，其余节点会被丢弃
- 模板里的策略组 / 规则内容不能在「模板」页直接编辑，要套用到某个订阅组改好后用同名另存覆盖（Parser 模板可以直接编辑）。已列入 [ROADMAP.md](ROADMAP.md)
- 会话存内存，重启即失效；不支持多实例共享
- 规则集只是生成 `rule-providers` 让客户端自己下载，服务端不下载也不校验规则 URL 是否有效
- `text` / `mrs` 格式的规则集和部分节点类型需要 mihomo（Clash Meta）内核，原版 Clash 不支持
- `validate` 不解析逻辑规则（`AND` / `OR` / `NOT`）里的策略名
- 服务端会访问管理员填写的任意 http(s) 地址（订阅、解锁地址），没有内网地址限制；管理密码泄露等同于可从服务器发起请求
