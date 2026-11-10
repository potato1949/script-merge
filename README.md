# script-merge

Clash 订阅合并服务：把多个订阅的节点合并，套上自己的策略组和规则，生成新的订阅链接。Go 单二进制，网页管理，Docker 部署。

## 功能

- **多个订阅组**：每个订阅组有自己的节点来源、策略组、规则、Parser 和订阅链接，互不影响
- **模板**：策略组、规则、Parser 都可以存为模板，再套用到其他订阅组（复制一份，之后各改各的）；内置几套常用模板
- **节点来源**：远程订阅（Clash YAML，或 base64 / 明文分享链接），以及手动填写的节点（YAML 或 ss、vmess、trojan、vless、hysteria2 分享链接）
- **UA 透传或固定**：每个订阅可选择透传客户端的 User-Agent（客户端用什么 UA 拉合并链接，就用什么 UA 拉上游），或使用固定 UA。固定 UA 在全局设置里填写，所有订阅共用（内置主流客户端预设）
- **订阅解锁**：可配置「解锁地址」，每次拉取订阅前先访问一次（普通 GET，适用于「访问过某网址才放行」的订阅）。默认用 Chrome 浏览器的 UA，可在全局设置里修改
- **节点处理**：按来源加前缀、正则保留 / 排除、重名自动加序号
- **策略组**：手动选择 / 自动测速 / 故障转移 / 负载均衡，成员可按正则筛选节点
- **第三方规则**：以 rule-providers 方式引入，内置 blackmatrix7、Loyalsoldier、ACL4SSR、MetaCubeX 预设，也可填任意 URL；另有前置 / 后置自定义规则
- **CFW Parsers**：支持 `prepend-*` / `append-*` / `mix-*` / `commands` 语法，对最终配置做任意微调
- **缓存与容错**：上游结果按「订阅 + 实际链接 + 实际 UA」分别缓存（默认 5 分钟），同一 UA 高频拉取不会重复请求上游；拉取失败时只回退到同一链接和 UA 上次成功的结果；全部失败时返回 502 而不是空配置
- **管理密码**：首次访问时设置，或用环境变量固定

## 部署

```bash
docker compose up -d --build
```

打开 `http://服务器IP:8080`，首次访问设置管理密码。数据保存在 `./data`。

也可以直接运行：

```bash
go build -o script-merge . && ./script-merge
```

### 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `PORT` | `8080` | 监听端口 |
| `DATA_DIR` | `./data`（镜像内为 `/data`） | 配置与缓存目录 |
| `ADMIN_PASSWORD` | 空 | 设置后以它作为管理密码，网页端不可修改；不设置则首次访问时在网页设置 |

> 管理页面和订阅链接都是明文 HTTP。暴露到公网时请放在 HTTPS 反向代理（Nginx / Caddy）后面。
> 订阅链接里的令牌等同于密码，泄露后在该订阅组的「本组设置」里重置即可。

## 使用

1. 左栏选择一个**订阅组**，或新建（新建时可以从模板带入策略组和规则）
2. **订阅源**：添加订阅链接或手动节点。订阅链接填机场的 Clash 订阅链接（或分享链接列表），可选择透传 UA 或使用全局设置里的固定 UA；需要先访问某个页面才能拉取的，填写「解锁地址」
3. **策略组 / 规则 / Parser**：直接编辑，或点「套用模板」；配置好的内容可以「存为模板」给其他订阅组用
4. **预览**：填一个要模拟的客户端 UA，点「生成预览」检查结果和警告
5. 点右上角「复制订阅链接」，填入客户端

输出始终是 Clash 格式（Stash 可直接使用）：UA 透传只决定向上游要节点时用什么身份。

订阅链接后加 `?refresh=1` 可跳过缓存强制重新拉取上游。

### Parser 示例

```yaml
yaml:
  prepend-rules:
    - DOMAIN,test.com,DIRECT
  append-proxies:
    - {name: test, type: http, server: 1.2.3.4, port: 456}
  commands:
    - dns.enable=false                              # 覆盖
    - proxy-groups.0.proxies.2+DIRECT               # 在第 1 个策略组的第 3 个位置插入
    - proxy-groups.(🚀 节点选择).proxies=[]proxyNames|香港  # 用匹配的节点覆盖
    - proxies.0-                                    # 删除第 1 个节点
```

Parser 在节点、策略组、规则都生成之后执行，可以引用本组的全部节点（含手动节点）。完整语法见 [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md#cfw-parser)。仅支持 YAML 形式，不支持 CFW 的 JavaScript 形式（`code` / `file` / `remote`）。

## 升级

配置结构变化时会在启动时自动迁移，迁移前的文件保留为 `data/config.json.bak`。

## 开发

见 [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md)。后续版本的开发方向见 [docs/ROADMAP.md](docs/ROADMAP.md)。
