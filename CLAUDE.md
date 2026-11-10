# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目

Clash 订阅合并服务：拉取多个上游订阅的节点，套上策略组、规则和 CFW Parser，按「订阅组」各自输出一个 Clash 格式的订阅链接。Go 单二进制 + 内嵌的单文件网页管理界面，数据存 JSON 文件。

代码注释、界面文案、文档都是中文，保持一致。

详细的设计说明在 `docs/DEVELOPMENT.md`（数据流、缓存规则、迁移、Parser 语法、常见扩展的改法）。改动涉及这些内容时同步更新它和 `README.md`。

后续要做的功能和已确认不做的事项在 `docs/ROADMAP.md`；实现了其中某项后把它从路线图里移除。

## 命令

```bash
go run .                          # 启动，http://localhost:8080，数据在 ./data
go test ./...                     # 全部测试（用 httptest 模拟上游，不访问外网）
go test -run TestBuildEndToEnd .  # 单个测试
go test -count=20 ./...           # 改了并发 / 缓存相关代码后多跑几轮，测试曾出现过竞态
go vet ./... && gofmt -l .        # 提交前检查；gofmt -l 应无输出
docker compose up -d --build      # 容器部署（Dockerfile 尚未实际构建验证过）
```

环境变量：`PORT`（默认 8080）、`DATA_DIR`（默认 `./data`）、`ADMIN_PASSWORD`（设置后网页端不可改密码）。

前端没有构建步骤，但 `web/index.html` 是编译期 `embed` 进二进制的，改完要重新 `go run .`。改完至少做一次语法检查：

```bash
node -e 'const s=require("fs").readFileSync("web/index.html","utf8");new Function(s.match(/<script>([\s\S]*)<\/script>/)[1])'
```

Windows 下服务在运行时 `script-merge.exe` 被占用，重新编译前先停掉进程。

## 架构

单个 `package main`，无框架，依赖只有 `yaml.v3` 和 `regexp2`。

### 术语（容易混）

- **订阅组** = `Profile`：本服务的概念，一个订阅组对应一个输出链接（`/sub/{token}`）
- **策略组** = `Group`：Clash 的 proxy-group

### 数据模型（`store.go`）

`Config` 下有 `Profiles[]`（彼此完全独立，各自带节点来源 `Subscriptions`、`Groups`、内嵌的 `Rules`、`Parser`）、`Templates[]`，以及全局的 `BaseConfig` / `CacheMinutes` / `UpdateInterval` / `UserAgent`（固定 UA）/ `UnlockUserAgent`（解锁 UA）。

- 节点来源 `Subscription.Kind`：`remote`（远程订阅）或 `manual`（手填节点）；空值等同 `remote`
- **模板是复制语义**：套用 = 把模板内容复制进订阅组，之后互不影响。服务端只存 `Templates`，套用和另存完全是前端对 `cfg` 对象的操作，`Build` 不知道模板的存在
- 内置模板（`seedTemplates`）只在新建配置或迁移时写入一次。两套策略组模板和两套规则模板共用同一批策略组名（`gProxy` 等常量）才能任意组合，新增内置模板要沿用这些名字（`TestSeedTemplatesAreConsistent` 会检查）

### 构建流程（`build.go` 的 `Build`）

`GET /sub/{token}` 和 `GET /api/preview` 调同一个 `Build(cfg, profile, fetcher, clientUA, force)`：

基础配置 → 并发处理节点来源 → 过滤 / 加前缀 / 重名加序号 → 生成策略组 → 生成规则 → `ApplyParser` → `validate`（只产生警告）→ `toNode` 输出 YAML。

- `clientUA` 是来拉合并链接的客户端的 UA。`resolveUA` 用它决定每个订阅本次拉上游用哪个 UA：订阅的 `UAMode` 是透传就用 `clientUA`，否则用全局的固定 UA（`Config.UserAgent`，空则 `defaultUA`）。每个订阅只有一个上游链接，也没有自己的 UA
- 上游内容只认 Clash YAML 或分享链接列表；输出始终是 Clash YAML，不随客户端变化。其他客户端格式的方向见 `docs/CLIENT-SUPPORT.md`（加输出侧的渲染器，不加输入解析器）
- 内存里的配置树统一用 `map[string]any` / `[]any`，不要放 `[]map[string]any` 或 `[]string`：Parser 的路径命令和 `toNode` 只认前者
- 面向用户的正则一律走 `compileRegex` / `optRegex`（regexp2，支持前瞻断言，带匹配超时），不要用标准库 `regexp`

### 上游拉取与缓存（`fetch.go`）

- 缓存键 = 订阅 ID + hash(实际链接 + 实际 UA + 解锁地址，有解锁地址时再加解锁 UA)。加了影响拉取结果的字段（不论在 `Subscription` 还是全局），必须加进 `cacheKey`
- 失败时只回退到**同一个键**的旧缓存，不跨链接 / UA 回退（这是用户明确要求的行为）
- 解锁地址是一次独立的普通 GET，UA 用全局的解锁 UA（`Config.UnlockUserAgent`，空则 `chromeUA`），不随订阅的 UA 模式变化，不与订阅请求共享 Cookie（同样是用户明确要求）
- 订阅 ID 用作缓存文件名，`validateConfig` 只接受服务端生成的 16 位十六进制 ID

### 配置保存与令牌（`main.go`）

前端每次保存都是把整个 `cfg` PUT 回 `/api/config`，没有局部更新接口。`validateConfig` 负责校验和规范化：ID 由服务端生成；各订阅组的 `Token` 按 ID 找回原值，前端传来的一律忽略，只能通过 `POST /api/token` 重置。

### 迁移（`store.go` 的 `Config.migrate`）

`Config.Version` 是结构版本（当前 `configVersion = 4`）。`OpenStore` 时自动迁移，原文件存为 `config.json.bak`。

- 只是**加字段**不需要迁移，但零值必须是合理的默认行为
- **改结构**：`configVersion` 加一，在 `migrate` 里加一段，旧字段以 `Legacy*` + `omitempty` 保留，并在 `TestMigrateLegacyConfig` 补一个旧文件样例

### CFW Parser（`parser.go`）

实现 CFW Parsers 的 YAML 语法（`prepend-*` / `append-*` / `mix-*` / `commands`），不支持 JavaScript 形式。`commands` 的 `定位 + 操作 + 设定值` 解析在 `parseCommand`，执行在 `applyPath`（递归下降，每层返回修改后的节点）。单条命令失败只产生警告，不中断。

### 前端（`web/index.html`）

原生 JS，`h()` 建元素，`render()` 整页重绘，状态是几个模块级变量（`cfg`、`page`、`pi`、`tab`、`dirty`）。

- `field(label, obj, key, opts)`：绑定到 `obj[key]` 的控件，输入即写回并标记未保存；不属于配置的临时输入传 `local: true`
- `item(obj, opts)`：可展开的行，展开状态按对象引用记在 `openItems`
- **只有结构变化才调 `render()`**（增删、排序、切换类型）。普通文本输入不要重绘，否则输入框会失焦
- 子元素数组可以任意嵌套，`h()` 会完全展平
- 服务端新增的数组 / 字符串字段要在 `normalize()` 里补默认值
- 配色集中在文件顶部的 CSS 变量（浅色、深色两组），风格参考 Claude 官网（象牙白底、陶土橙强调色、衬线标题）

界面没有自动化测试。改界面后，用无头浏览器 + 模拟的 `fetch` 截图检查渲染（把页面复制一份，在主脚本前注入返回固定数据的 `window.fetch`，再用 `msedge --headless=new --screenshot` 截图）。之前有一次渲染 bug 就是这样发现的。
