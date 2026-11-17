package main

import (
	"fmt"
	"net/netip"
	"reflect"
	"strings"
)

// 这里处理上游（机场）的 Clash 配置里节点以外的部分：底板的策略组，以及其他来源的节点要带上的 hosts。

// nodeHosts 找出和这批节点的地址解析有关的 hosts 与 fake-ip-filter 条目。
// 有的机场让节点地址经 hosts 映射到另一个域名（可能不止一层），再把这些域名放进 fake-ip-filter；
// 只合并节点本身时这层映射丢了，节点会连不上。所以从节点的 server 出发，顺着 hosts 一层层找下去，
// 把沿途用到的 hosts 条目和能匹配到这些域名的 fake-ip-filter 条目带上；和节点无关的（*.lan 之类）不带。
func nodeHosts(up *upstream, servers []string) (map[string]any, []string) {
	upHosts, upFilter := up.hosts(), up.fakeIPFilter()
	if len(upHosts) == 0 && len(upFilter) == 0 {
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
		for k, v := range upHosts {
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
	for _, f := range upFilter {
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

// mergeNodeHosts 把 nodeHosts 找出的条目合并进输出。底板里已有的同名 hosts 以底板为准；
// fake-ip-filter 只在底板有 dns 段、且不是白名单模式时才追加（白名单模式下追加会把意思弄反）。
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
					warns = append(warns, fmt.Sprintf("hosts「%s」和底板机场里的不同，用了底板里的，相关节点可能连不上", k))
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

// baseGroups 把底板机场的策略组转成输出里的策略组。
// rename：底板节点名 → 输出里的名字（加前缀、去重名之后；被过滤掉的节点没有对应）。
// 某个策略组包含了底板的全部节点时，把它当作「全部节点」的组，其他来源的节点（others）也加进去；
// 只包含部分节点的（比如按地区分的）只换名字。include-all、filter、use 这类写法原样保留，由客户端处理。
func baseGroups(up *upstream, rename map[string][]string, others []string) (groups []any, warns []string) {
	upNodes := map[string]bool{}
	for _, p := range up.Proxies {
		upNodes[fmt.Sprint(p["name"])] = true
	}
	for _, g := range up.groups() {
		gm := deepCopy(g).(map[string]any)
		name := fmt.Sprint(gm["name"])
		if members, ok := gm["proxies"].([]any); ok {
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
				for _, n := range others {
					put(n)
				}
			}
			gm["proxies"] = out
		}
		_, hasUse := gm["use"]
		if ps, _ := gm["proxies"].([]any); len(ps) == 0 && !hasUse && gm["include-all"] != true && gm["include-all-proxies"] != true && gm["filter"] == nil {
			gm["proxies"] = []any{"DIRECT"}
			warns = append(warns, fmt.Sprintf("底板的策略组「%s」里的节点都被过滤掉了，已填入 DIRECT", name))
		}
		groups = append(groups, gm)
	}
	return groups, warns
}
