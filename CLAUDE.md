# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目

Clash 订阅合并服务：每个「订阅组」选一个机场作为**底板**，输出它的完整 Clash 配置（策略组、规则、DNS 等原样保留），再把其他机场和手动节点并进去，生成一个订阅链接。Go 单二进制 + 内嵌的单文件网页管理界面，数据存 JSON 文件。

代码注释、界面文案、文档都是中文，保持一致。

详细的设计说明在 `docs/DEVELOPMENT.md`（设计取舍、数据流、缓存规则、Parser 语法、常见扩展的改法）。改动涉及这些内容时同步更新它和 `README.md`。

后续要做的功能和已确认不做的事项在 `docs/ROADMAP.md`；实现了其中某项后把它从路线图里移除。

## 方向（重要）

v8 重构时用户确定**只保留底板方案**：策略组和规则只来自底板机场，不再提供策略组 / 规则编辑器、模板、第三方规则集、规则仓库、全局基础配置、UA 透传、历史版本、导出导入，也不做配置迁移。之前两套方案（自建 vs 沿用机场）叠在一起导致项目混乱，所以新需求不要再把「自己维护策略组 / 规则」加回来；微调一律靠 Parser。旧实现存档在 `archive/follow-mixed` 分支。

## 命令

```bash
go run .                          # 启动，http://localhost:8080，数据在 ./data
go test ./...                     # 全部测试（用 httptest 模拟上游，不访问外网）
go test -run TestBuildEndToEnd .  # 单个测试
go test -count=20 ./...           # 改了并发 / 缓存相关代码后多跑几轮，测试曾出现过竞态
go vet ./... && gofmt -l .        # 提交前检查；gofmt -l 应无输出
docker compose up -d --build      # 容器部署（Dockerfile 尚未实际构建验证过）
```

环境变量：`PORT`（默认 8080）、`DATA_DIR`（默认 `./data`）、`ADMIN_PASSWORD`（设置后网页端不可改密码）、`HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`（上游拉取的代理，标准库 `http.DefaultTransport` 直接读取）。

前端没有构建步骤，但 `web/index.html` 是编译期 `embed` 进二进制的，改完要重新 `go run .`。改完至少做一次语法检查：

```bash
node -e 'const s=require("fs").readFileSync("web/index.html","utf8");new Function(s.match(/<script>([\s\S]*)<\/script>/)[1])'
```

Windows 下服务在运行时 `script-merge.exe` 被占用，重新编译前先停掉进程。Windows 上 `time.Now()` 精度有限，测试里不要断言两次调用的时间严格递增。

## 架构

单个 `package main`，无框架，依赖只有 `yaml.v3` 和 `regexp2`。

### 术语（容易混）

- **订阅组** = `Profile`：本服务的概念，一个订阅组对应一个输出链接（`/sub/{token}`）
- **策略组**：Clash 的 proxy-group，只来自底板
- **底板** = `Profile.Base`：订阅组选定的一个远程来源，输出以它的完整配置为准

### 数据模型（`store.go`）

`Config` 下有 `Profiles[]`（各自带 `Base`、`Parser` 和对节点来源的引用 `Sources []SourceRef`）、全局的节点来源 `Sources[]`，以及全局的 `CacheMinutes` / `UpdateInterval` / `UserAgent`（拉取 UA）/ `UnlockUserAgent`（解锁 UA）。

- **节点来源是全局对象，订阅组只存引用**：`Source` 放属于来源本身的内容（名称、`Kind`、链接、解锁地址、手填节点），`SourceRef` 放「这个订阅组怎么用它」（启用、前缀、Include / Exclude、前置代理 `Dialer`）。同一个来源可以被多个订阅组引用，也可以在一个组里引用多次。给来源加字段时先想清楚它属于哪一边
- `Source.Kind`：`remote`（远程订阅）或 `manual`（手填节点）；空值等同 `remote`
- **底板**：`Profile.Base` 必须是本组引用的远程来源；同一来源被引用多次时第一条是底板，底板不看启用开关。`validateConfig`（服务端）和 `fixBase`（前端）按同一规则修正：不合法就改用本组第一个远程来源，没有则为空
- **不做迁移**：`Config.Version`（当前 `configVersion = 8`）对不上时 `OpenStore` 把原文件存为 `config.json.bak`、换成默认配置、保留管理密码。加字段不用改版本（零值必须是合理的默认行为）；改结构才加版本，旧配置会整个作废

### 构建流程（`build.go` 的 `Build`）

`GET /sub/{token}` 和 `POST /api/preview` 调同一个 `Build(cfg, profile, fetcher, mode)`（`FetchMode`：正常 / 跳过缓存 / 只用已有缓存）：

本组用到的来源（底板排第一）并发拉取 → 底板拿不到或没有策略组就报错 → `root` = 底板配置去掉 proxies 的深拷贝 → 汇总节点（底板的在前；过滤 / 加前缀 / 重名加序号 / 写 dialer-proxy；非底板来源带上节点地址用到的 hosts）→ `baseGroups` 改写底板的策略组 → `ApplyParser` → `validate`（只产生警告）→ `toNode` 输出 YAML。

- **预览不保存**：`POST /api/preview` 的请求体带着前端当前的整份 `cfg`（可能还没保存）和订阅组下标，服务端先过 `validateConfig` 再 `Build`，不落盘。「先预览、满意再保存」是界面的基本流程，不要让预览依赖已保存的配置
- **底板原样保留**：规则、`rule-providers`、`proxy-providers`、DNS、hosts 都不动。`baseGroups`（`upstream.go`）只把成员里的底板节点名换成输出名、去掉被过滤的；包含底板全部节点的组追加其他来源的节点；`use` / `include-all` / `filter` 原样保留
- 代理链只有一种做法：`SourceRef.Dialer` 给这条引用的节点写上 `dialer-proxy`。前置代理里含有这些节点本身会成环，`validateDialers` 只警告，不自动排除（这是用户确认的取舍）
- 上游内容只认 Clash YAML 或分享链接列表；输出始终是 Clash YAML。其他客户端格式的方向见 `docs/CLIENT-SUPPORT.md`（加输出侧的渲染器，不加输入解析器）
- 内存里的配置树统一用 `map[string]any` / `[]any`，不要放 `[]map[string]any` 或 `[]string`：Parser 的路径命令和 `toNode` 只认前者
- **节点地址的 hosts**（`upstream.go` 的 `nodeHosts`）：非底板来源的节点 server 经 hosts 映射（可能多层）时，找出要用的上游 hosts 和 fake-ip-filter 条目合并进输出；底板自己的已经整份在输出里
- 面向用户的正则一律走 `compileRegex` / `optRegex`（regexp2，支持前瞻断言，带匹配超时），不要用标准库 `regexp`

### 上游拉取与缓存（`fetch.go`）

- 拉取 UA 只有全局的 `Config.UserAgent`（空则 `defaultUA`），没有透传
- 缓存键 = 来源 ID + hash(链接 + UA + 解锁地址，有解锁地址时再加解锁 UA)，所以引用同一个来源的订阅组共用缓存。加了影响拉取结果的字段（不论在 `Source` 还是全局），必须加进 `cacheKey`；`SourceRef` 上的字段只影响拉到之后怎么用，不进缓存键
- 失败时只回退到**同一个键**的旧缓存，不跨链接 / UA 回退（这是用户明确要求的行为）
- 解锁地址是一次独立的普通 GET，UA 用全局的解锁 UA（`Config.UnlockUserAgent`，空则 `chromeUA`），不与订阅请求共享 Cookie（同样是用户明确要求）
- 来源 ID 用作缓存文件名，`validateConfig` 只接受 16 位十六进制的 ID（前端新建来源、订阅组时用 `newID()` 按同样格式生成，这样保存前就能被引用）
- 缓存里存节点和上游配置去掉 proxies 后的整份内容（`upstream.Doc`，键统一转成字符串）。改了缓存里存什么时 `cacheFormat` 加一，旧格式的缓存视为过期
- `Fetcher.state` 记下每个来源最近一次**实际请求上游**的结果（`SourceState`：时间、错误、上次成功的时间 / 节点数 / `Subscription-Userinfo` / 策略组名），`GET /api/source-status` 给界面显示（状态标签、流量、前置代理候选）。只在内存里，重启后从磁盘缓存恢复成功的部分
- 代理只走环境变量：`fetchSubscription` 的 `http.Client` 没有自定义 `Transport`，落在 `http.DefaultTransport` 上。以后换自定义 `Transport` 要带上 `Proxy: http.ProxyFromEnvironment`（`TestFetchUsesProxy` 会检查）。标准库对回环地址从不走代理，且只在第一次请求时读一次环境变量，所以测试在 `TestMain` 里设置代理

### 配置保存与令牌（`main.go`）

前端每次保存都是把整个 `cfg` PUT 回 `/api/config`，没有局部更新接口。`Config.Revision` 每保存一次加一，PUT 带回的值和当前的对不上就返回 409（别的页面先保存过），前端询问后带 `?force=1` 覆盖；服务端内部的保存（重置令牌）不检查。`validateConfig` 负责校验和规范化：格式不对或重复的 ID 重新生成（来源的 ID 变了，订阅组里的引用和底板跟着改）；引用了不存在的来源直接去掉；修正底板；各订阅组的 `Token` 按 ID 找回原值，前端传来的一律忽略，只能通过 `POST /api/token` 重置。

### 程序版本（`version.go`）

左栏底部显示的程序版本：`-ldflags "-X main.version=…"` 指定的优先，否则取 `debug.ReadBuildInfo` 里的 tag（当前提交正好在 tag 上时）或 git 提交（`go build` 自动带上，`go run` 和 Docker 构建没有，显示「开发版」）。

版本号是 `main` 上带注释的 git tag `vX.Y.Z`（从 v1.0.0 开始），变更规则见 `docs/DEVELOPMENT.md` 的「版本号」：不兼容（如 `configVersion` 加一）加主版本，新功能加次版本，修复加修订号；只改文档 / 测试不发版。只在用户要求时打 tag。只在已登录时随 `GET /api/status` 返回。和 `Config.Version`（配置结构版本）、`Config.Revision`（保存次数）是三回事。

### 访问记录（`access.go`）

存在 `DATA_DIR/access.json`，不属于配置。`handleSub` 每次拉取（含失败）记一条。客户端 IP 的取法见 `clientIP`：只有直接对端是本机 / 内网地址时才看 `X-Forwarded-For`。

### CFW Parser（`parser.go`）

实现 CFW Parsers 的 YAML 语法（`prepend-*` / `append-*` / `mix-*` / `commands`），不支持 JavaScript 形式。`commands` 的 `定位 + 操作 + 设定值` 解析在 `parseCommand`，执行在 `applyPath`（递归下降，每层返回修改后的节点）。单条命令失败只产生警告，不中断。它是用户调整底板内容的唯一手段。

### 前端（`web/index.html`）

原生 JS，`h()` 建元素，`render()` 整页重绘，状态是几个模块级变量（`cfg`、`page`、`pi`、`tab`、`dirty`、`srcStatus`、`previews`、`accessLog`）。页面：订阅组（标签页：节点来源 / Parser / 预览 / 本组设置）、节点来源、全局设置。

- 来源的新建、修改、删除只在「节点来源」页（`sourcesPage` + `sourceFields`），免得在一个组里顺手一改、别的组跟着变。订阅组的节点来源标签页（`tabSubs`）只能选用已有来源、移除引用、设为底板、改 `SourceRef` 上的字段，来源内容只读展示；增删引用后调 `fixBase`。用户明确要求：订阅组里不放新建来源、跳转到「节点来源」页的入口，也不放底板的策略组 / 规则说明
- 界面上不加解释性的说明文字（用户明确要求），状态、错误、警告这类必要信息除外
- 订阅组的 ID 由前端 `newID()` 生成，预览结果 `previews` 按它存；判断「还没保存过」看 `p.token` 是否为空
- `field(label, obj, key, opts)`：绑定到 `obj[key]` 的控件，输入即写回并标记未保存；不属于配置的临时输入传 `local: true`
- `item(obj, opts)`：可展开的行，展开状态按对象引用记在 `openItems`
- **只有结构变化才调 `render()`**（增删、排序、换底板）。普通文本输入不要重绘，否则输入框会失焦
- 子元素数组可以任意嵌套，`h()` 会完全展平
- 服务端新增的数组 / 字符串字段要在 `normalize()` 里补默认值
- 配色集中在文件顶部的 CSS 变量（浅色、深色两组），风格参考 Claude 官网（象牙白底、陶土橙强调色、衬线标题）

界面没有自动化测试。改界面后，用无头浏览器实际检查：起一个临时 `DATA_DIR` 的真实服务和一个返回固定内容的假上游（如 `python -m http.server` 托管几个 YAML），用 `msedge --headless=new --remote-debugging-port=…` 通过 CDP 执行点击并断言 `cfg` 的状态（顶层的 `let` / `const` 在 `Runtime.evaluate` 里可以直接访问），再截图看一眼。
