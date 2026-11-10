# 多客户端输出：设计结论

> 状态：**方向已定**（2026-11-11）。Stash 直接使用现有的 Clash 输出，不需要改动；非 mihomo 系客户端（Loon 等）等真正需要时再做，做法见「留的口子」。
> 标「待核实」的内容还没有查文档或实机确认。

## 背景与目标

- 桌面主力是 Clash（mihomo 内核）。iOS 打算从小火箭换成 Stash，以后可能用 Loon 或其他客户端。
- 目标：同一个订阅组在不同客户端上用**同一套节点、策略组、规则**，而不是每个客户端各配一套。
- 暂不考虑：sing-box、Surge、Quantumult X、小火箭的 conf 格式。

## 已定的结论

### 1. 内部表示就是 Clash 配置树

节点是 mihomo 字段名的 `map[string]any`，整份配置是 `Build` 里的 `root`。mihomo 是协议覆盖最全、机场普遍提供的格式，现有的过滤、前缀、策略组筛选、Parser 都建立在它上面。其他客户端的格式在输出时由它转换，不另外设计中间结构。

### 2. 上游统一按 Clash 格式拉取

所有客户端来拉合并链接，服务端向上游要的都是同一种内容：Clash YAML，或分享链接列表。**不按客户端去拉它的原生格式。**

理由：

- 手填节点和分享链接来源本来就不是目标客户端的格式，「Clash 节点 → 目标格式」的转换无论如何都要写。
- 过滤、加前缀、重名加序号、策略组筛选都要读懂节点，原生格式的节点行不能原样透传。
- 不是每家机场都提供每种客户端的格式。
- 按原生格式拉意味着每种格式多一个输入解析器，同一个机场在不同客户端上的节点还可能不一致。

拉原生格式唯一的好处是机场替客户端去掉不支持的节点。这一点以后由输出时的能力过滤代替（见「留的口子」）。

### 3. 「专用链接」已移除

原先的 `Subscription.Variants`（按来拉合并链接的客户端 UA 切换上游链接）是为「按客户端拉原生格式」准备的，结论 2 之后没有用途，已在配置结构 v4 中移除。每个订阅只有一个链接，填 Clash 订阅链接即可。

UA 透传（`UAMode = passthrough`）保留：它只影响向上游报什么 UA，不改变「上游内容必须是 Clash YAML 或分享链接」这个约定。

### 4. Stash 直接用 Clash 输出

Stash 的配置就是 Clash 格式的 YAML，现有输出可以直接用，不需要单独的渲染器、基础配置或规则集链接。差异只在少数规则类型上，见下一节。

## Stash 与 mihomo 的规则差异（2026-11-11 核实）

依据是两边的官方文档。「Stash 文档没提」只代表文档没写，不等于确认不支持。

### 规则类型

| 情况 | 规则类型 |
|---|---|
| 两边都支持 | `DOMAIN`、`DOMAIN-SUFFIX`、`DOMAIN-KEYWORD`、`DOMAIN-WILDCARD`、`DOMAIN-REGEX`、`GEOSITE`、`GEOIP`、`IP-CIDR`、`IP-CIDR6`、`IP-ASN`、`DST-PORT`、`SRC-PORT`、`NETWORK`、`RULE-SET`、`AND` / `OR` / `NOT`、`MATCH` |
| mihomo 有，Stash 文档没提 | `SUB-RULE`、`IP-SUFFIX`、`SRC-IP-CIDR`、`SRC-GEOIP`、`SRC-IP-ASN`、`SRC-IP-SUFFIX`、`IN-PORT`、`IN-TYPE`、`IN-USER`、`IN-NAME`、`REMATCH-NAME`、`UID`、`DSCP`、`PROCESS-*-WILDCARD`、`PROCESS-*-REGEX` |
| Stash 在 iOS / tvOS 上明确忽略 | `PROCESS-NAME`、`PROCESS-PATH`（配置里可以有，不生效） |
| Stash 有，mihomo 没有 | `SCRIPT`、`USER-AGENT`、`URL-REGEX`、`PROTOCOL`、`SRC-IP`、`GEOIP,UNKNOWN`、`IP-ASN,UNKNOWN` |

细节：

- 匹配来源 IP：mihomo 用 `SRC-IP-CIDR`，Stash 用 `SRC-IP`（iOS 3.7+，Beta），名字不同。
- `SRC-PORT` 在 Stash 上要 iOS 3.7+，也是 Beta。
- `MATCH` 在 Stash 的类型列表里没有单独列出，但官方示例里在用。
- `GEOSITE`：Stash 不内置数据，首次使用时从 github.com 下载。
- `GEOIP`：Stash 用 MaxMind 格式的库，按国家代码匹配。

### 规则选项

| 选项 | mihomo | Stash |
|---|---|---|
| `no-resolve` | 支持 | 支持 |
| `src`（把目标 IP 规则改成匹配来源 IP） | 支持 | 文档没提 |
| `no-track`、`pre-matching` | 没有 | Stash 独有 |

### 规则集（rule-providers）

| 项目 | mihomo | Stash |
|---|---|---|
| `behavior` | domain / ipcidr / classical | 相同 |
| `format` | yaml / text / mrs | 相同 |
| mrs 的限制 | 只支持 domain 和 ipcidr | 相同；App Store 更新说明显示 3.2.0 起支持 |
| classical 里能放什么 | 除 `RULE-SET`、`SUB-RULE` 外的所有类型 | 文档写「任意规则类型」 |
| 内联规则集（`type: inline` + `payload`） | 支持 | iOS 3.6+ 支持 |
| 自定义请求头 | 字段名是 `header` | 字段名是 `headers` |
| `proxy`、`size-limit`、`path-in-bundle` | 支持 | 文档没提 |
| `path` | 限制在 HomeDir 内 | 限制在 Stash 管理的资源目录内 |

### 对本项目的影响

- `build.go` 生成的是 `RULE-SET` 规则加 `type: http` 的规则集（`behavior`、`format`、`url`、`path`、`interval`）；内置模板用 classical / domain / ipcidr 的 yaml 规则集，加 `GEOIP,CN` 和 `MATCH`。这些都在两边的交集里，**不需要为 Stash 改任何输出**。
- 风险只在用户手写的前置 / 后置规则和 Parser 加进来的内容：其中若有 mihomo 独有的类型（最可能是 `SUB-RULE`、`SRC-IP-CIDR`），在 Stash 上可能有问题。
- 可选的后续改进：客户端 UA 是 Stash 时，对上表「mihomo 有，Stash 文档没提」的规则行给出警告。是否直接删掉这些行，等下面的实机测试结果再定。

### 资料来源

- Stash：[规则类型](https://stash.wiki/rules/rule-types)、[规则集合](https://stash.wiki/rules/rule-set)（文档更新于 2026-10-15）、[iOS 更新说明](https://stash.wiki/en/release-notes/ios)
- mihomo：[路由规则](https://wiki.metacubex.one/config/rules/)、[规则集合](https://wiki.metacubex.one/config/rule-providers/)、[规则集合内容](https://wiki.metacubex.one/config/rule-providers/content/)

## 留的口子：以后接入非 mihomo 系客户端

以 Loon 为例。要加的是输出侧的**渲染器**（把 Clash 配置树转成目标客户端的配置文本），输入侧不用动：上游仍然只按 Clash 格式拉。

### 接入位置

`Build` 的前 6 步（基础配置 → 节点 → 策略组 → 规则 → Parser → validate）产出 Clash 配置树 `root`，第 7 步把它编码成 YAML。渲染器接在第 6 步之后，替换第 7 步：

- Clash / Stash：现在的 `toNode` + YAML 编码。
- Loon：从 `root` 里取 `proxies`、`proxy-groups`、`rules`、`rule-providers`，转成 Loon 的 `[Proxy]`、`[Proxy Group]`、`[Rule]`、`[Remote Rule]` 等段；`root` 里其余的 Clash 配置项（`dns`、`tun` 等）不转换。

这样 Parser 对节点、策略组、规则的修改在所有客户端上都生效。现在只有一种输出，代码里不预先加渲染器接口，等写第二个渲染器时再把第 7 步拆出来，改动只在 `Build` 末尾。

### 加一个客户端要做的事

1. **渲染器**：节点、策略组、规则行、规则集的转换。目标客户端不支持的协议、字段、规则类型跳过，并在预览里给出警告（「n 个节点因 Loon 不支持 xxx 被跳过」）。
2. **输出目标的判定**：链接参数（如 `/sub/{token}?target=loon`）优先，其次按客户端 UA 自动识别，默认 Clash。预览页相应增加「目标客户端」选项。
3. **透传 UA 的限制**：透传只对 Clash 系客户端生效；非 Clash 系客户端来拉时退回固定 UA，否则机场会按 UA 返回该客户端的原生格式，解析不了。
4. **该客户端自己的基础配置**：例如 Loon 的 `[General]` 段。全局设置里按目标分别保存，Clash 的基础配置不参与转换。
5. **规则集链接**：其他客户端的规则集文件格式和 Clash 不同。`RuleSet` 加按目标的可选覆盖链接（只是加字段，不需要迁移）；没填时跳过该规则集并警告。内置模板补上对应链接。
6. **响应头**：`Content-Type`、文件扩展名，以及订阅流量信息的传递方式按目标调整。

缓存不受影响：拉取不依赖输出目标，缓存键不用改。

## 待核实

- **Stash**（现有输出已可用，以下只影响是否要加警告）：
  - 遇到不认识的规则类型，是跳过该行还是整份配置加载失败；
  - `./ruleset/xxx.yaml` 这种相对 `path` 是否可用；
  - 支持的协议清单（vless + reality、hysteria2、tuic、anytls 等）和不支持的 mihomo 配置项（tun、DNS、sniffer 等）。
- **Loon**（做的时候再查）：
  - 支持的协议清单和节点写法；
  - 策略组、远程规则的语法，规则类型的对应关系（如 `MATCH` 对应 `FINAL`）；
  - UA 字符串；
  - 订阅流量信息的传递方式。
