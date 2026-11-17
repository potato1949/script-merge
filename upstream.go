package main

import (
	"fmt"
	"net/netip"
	"reflect"
	"strings"
)

// 这里处理上游（机场）的 Clash 配置里节点以外的部分。

// nodeHosts 找出和这批节点的地址解析有关的 hosts 与 fake-ip-filter 条目。
// 有的机场让节点地址经 hosts 映射到另一个域名（可能不止一层），再把这些域名放进 fake-ip-filter；
// 只合并节点本身时这层映射丢了，节点会连不上。所以从节点的 server 出发，顺着 hosts 一层层找下去，
// 把沿途用到的 hosts 条目和能匹配到这些域名的 fake-ip-filter 条目带上；和节点无关的（*.lan 之类）不带。
func nodeHosts(up *upstream, servers []string) (map[string]any, []string) {
	if len(up.Hosts) == 0 && len(up.FakeIPFilter) == 0 {
		return nil, nil
	}
	hosts := map[string]any{}
	seen := map[string]bool{}
	var queue, domains []string
	push := func(d string) {
		d = strings.ToLower(strings.TrimSpace(d))
		if _, err := netip.ParseAddr(d); d == "" || err == nil || seen[d] {
			return
		}
		seen[d] = true
		queue = append(queue, d)
		domains = append(domains, d)
	}
	for _, s := range servers {
		push(s)
	}
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		for k, v := range up.Hosts {
			if !domainMatch(k, d) {
				continue
			}
			hosts[k] = v
			switch t := v.(type) {
			case string:
				push(t)
			case []any:
				for _, x := range t {
					push(fmt.Sprint(x))
				}
			}
		}
	}
	var filter []string
	for _, f := range up.FakeIPFilter {
		for _, d := range domains {
			if domainMatch(f, d) {
				filter = append(filter, f)
				break
			}
		}
	}
	return hosts, filter
}

// domainMatch 按 Clash 的域名通配写法判断 d 是否匹配 pattern：
// +.a.com 匹配 a.com 及其子域名，.a.com 只匹配子域名，* 匹配一级。geosite: / rule-set: 这类写法不匹配。
func domainMatch(pattern, d string) bool {
	p := strings.ToLower(strings.TrimSpace(pattern))
	switch {
	case strings.Contains(p, ":"):
		return false
	case strings.HasPrefix(p, "+."):
		return d == p[2:] || strings.HasSuffix(d, p[1:])
	case strings.HasPrefix(p, "."):
		return strings.HasSuffix(d, p)
	case strings.Contains(p, "*"):
		pl, dl := strings.Split(p, "."), strings.Split(d, ".")
		if len(pl) != len(dl) {
			return false
		}
		for i := range pl {
			if pl[i] != "*" && pl[i] != dl[i] {
				return false
			}
		}
		return true
	}
	return d == p
}

// mergeNodeHosts 把 nodeHosts 找出的条目合并进输出。基础配置里已有的同名 hosts 以基础配置为准；
// fake-ip-filter 只在基础配置有 dns 段、且不是白名单模式时才追加（白名单模式下追加会把意思弄反）。
func mergeNodeHosts(root map[string]any, h map[string]any, filter []string) (warns []string) {
	if len(h) > 0 {
		hosts, _ := root["hosts"].(map[string]any)
		if hosts == nil {
			hosts = map[string]any{}
			root["hosts"] = hosts
		}
		for k, v := range h {
			if old, ok := hosts[k]; ok {
				if !reflect.DeepEqual(old, v) {
					warns = append(warns, fmt.Sprintf("机场的 hosts「%s」和基础配置里的不同，用了基础配置里的，节点可能连不上", k))
				}
				continue
			}
			hosts[k] = v
		}
	}
	dns, _ := root["dns"].(map[string]any)
	if len(filter) == 0 || dns == nil || fmt.Sprint(dns["fake-ip-filter-mode"]) == "whitelist" {
		return warns
	}
	list, _ := dns["fake-ip-filter"].([]any)
	have := map[string]bool{}
	for _, x := range list {
		have[fmt.Sprint(x)] = true
	}
	for _, f := range filter {
		if !have[f] {
			have[f] = true
			list = append(list, f)
		}
	}
	dns["fake-ip-filter"] = list
	return warns
}

// followGroups 把上游的策略组转成输出里的策略组。上游策略组里的节点名换成本组输出里的名字
// （rename：上游节点名 → 加前缀、去重名后的名字；被过滤掉的节点没有对应）。
// 上游某个策略组包含了上游的全部节点时，当它是「全部节点」，本组其他来源的节点（allNodes）也加进去，
// 这样机场的「节点选择」里也能选到别的节点；只包含部分节点的（比如按地区分的）只换名字。
func followGroups(src string, up *upstream, rename map[string][]string, allNodes []string) (groups []map[string]any, warns []string) {
	upNodes := map[string]bool{}
	for _, p := range up.Proxies {
		upNodes[fmt.Sprint(p["name"])] = true
	}
	for _, g := range up.Groups {
		gm := deepCopy(g).(map[string]any)
		name := fmt.Sprint(gm["name"])
		if _, ok := gm["use"]; ok {
			delete(gm, "use")
			warns = append(warns, fmt.Sprintf("「%s」的策略组「%s」用到了机场的 proxy-providers，这部分没有带过来", src, name))
		}
		members, hasList := gm["proxies"].([]any)
		if hasList {
			out, added, nodes := []any{}, map[string]bool{}, 0
			put := func(n string) {
				if !added[n] {
					added[n] = true
					out = append(out, n)
				}
			}
			for _, m := range members {
				ms := fmt.Sprint(m)
				if !upNodes[ms] {
					put(ms) // 其他策略组、DIRECT、REJECT
					continue
				}
				nodes++
				for _, n := range rename[ms] {
					put(n)
				}
			}
			if nodes > 0 && nodes >= len(upNodes) {
				for _, n := range allNodes {
					put(n)
				}
			}
			gm["proxies"] = out
		}
		if ps, _ := gm["proxies"].([]any); len(ps) == 0 && gm["include-all"] != true && gm["include-all-proxies"] != true && gm["filter"] == nil {
			gm["proxies"] = []any{"DIRECT"}
			warns = append(warns, fmt.Sprintf("「%s」的策略组「%s」里的节点都被过滤掉了，已填入 DIRECT", src, name))
		}
		groups = append(groups, gm)
	}
	return groups, warns
}

// followRuleSet 是上游规则里一条 RULE-SET 对应的规则文件：http 类型的给出地址，inline 的直接给出内容。
type followRuleSet struct {
	name, policy, url string
	inline            []string
	err               string
}

// parseFollowRuleSet 解析上游的一条 RULE-SET 规则，找到它引用的 rule-provider。
func parseFollowRuleSet(up *upstream, rule string) followRuleSet {
	parts := strings.Split(rule, ",")
	rs := followRuleSet{}
	if len(parts) < 3 {
		rs.err = "格式不对"
		return rs
	}
	rs.name, rs.policy = strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2])
	prov, _ := up.RuleProviders[rs.name].(map[string]any)
	switch {
	case prov == nil:
		rs.err = "机场的配置里没有这个 rule-provider"
	case prov["type"] == "inline":
		for _, it := range stringsOf(prov["payload"]) {
			if r, ok := normRule(it); ok {
				rs.inline = append(rs.inline, r)
			}
		}
	case prov["type"] == "http" && fmt.Sprint(prov["url"]) != "":
		rs.url = strings.TrimSpace(fmt.Sprint(prov["url"]))
		if prov["format"] == "mrs" {
			rs.url, rs.err = "", "mrs 格式无法展开"
		}
	default:
		rs.err = fmt.Sprintf("类型 %v 无法展开", prov["type"])
	}
	return rs
}
