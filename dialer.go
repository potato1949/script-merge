package main

import (
	"fmt"
	"strconv"
	"strings"
)

// 前置代理（代理链）：节点的 dialer-proxy 指向一个策略组或节点，这个节点的连接经由它拨出。
// 两种来源：引用上直接填名字（SourceRef.Dialer，在 Build 汇总节点时写上），或者选「自动生成」（SourceRef.DialerAuto），
// 由 addAutoDialers 生成一个「<来源名> 前置」的 select 组再写上。不管哪种，最后都由 validateDialers 检查。
//
// 对流量走向的要求很严，这里的原则是：
//   - 自动生成的前置组结构上不能成环：带前置代理的节点一律不进前置组
//   - 前置组没有成员时填 REJECT（连不上），绝不填 DIRECT（落地节点会被直连，和用户的本意相反）
//   - 成环检查宁可多报不能漏报：按 mihomo 的规则展开 include-all / filter，拿不准的情况按「可能选到」算

// autoDialer 是一条选了「自动生成前置组」的引用。
type autoDialer struct {
	sub    int              // 在 BuildResult.Subs 里的下标
	source string           // 来源名，组名是「<来源名> 前置」
	filter string           // 节点正则，匹配输出名；空则全部
	nodes  []map[string]any // 这条引用在输出里的节点（和 proxies 里的是同一个对象）
}

// addAutoDialers 为每条自动生成前置组的引用追加一个 select 组，并给它的节点写上 dialer-proxy。
// proxies、groups 是到这一步为止的输出（Parser 之前）。成员是输出名匹配正则的节点（所有来源的，按输出顺序），
// 剔除所有带前置代理的节点：引用上填了前置的、节点自带的、以及自动生成前置的这些引用自己的节点。
// 组名和已有的策略组、节点或前面生成的前置组重名时加序号。
func addAutoDialers(defs []autoDialer, proxies []any, groups []any) (out []any, names []string, members [][]string, warns []string) {
	taken := map[string]bool{}
	for n := range builtinPolicies {
		taken[n] = true
	}
	for _, n := range namesOf(groups) {
		taken[n] = true
	}
	for _, n := range namesOf(proxies) {
		taken[n] = true
	}
	chained := map[string]bool{} // 带前置代理（或马上要带上）的节点
	for _, p := range proxies {
		if pm, _ := p.(map[string]any); pm != nil && hasDialer(pm) {
			chained[fmt.Sprint(pm["name"])] = true
		}
	}
	for _, d := range defs {
		for _, n := range d.nodes {
			chained[fmt.Sprint(n["name"])] = true
		}
	}
	for _, d := range defs {
		base := d.source + " 前置"
		name := base
		for i := 2; taken[name]; i++ {
			name = base + " " + strconv.Itoa(i)
		}
		taken[name] = true
		if name != base {
			warns = append(warns, fmt.Sprintf("「%s」和已有的策略组或节点重名，自动生成的前置组改叫「%s」", base, name))
		}
		list := []string{}
		re, err := optRegex(d.filter)
		if err != nil { // 正则写错时不猜，宁可一个成员都没有
			warns = append(warns, fmt.Sprintf("前置组「%s」的节点%v", name, err))
		} else {
			for _, n := range namesOf(proxies) {
				if !chained[n] && (re == nil || reMatch(re, n)) {
					list = append(list, n)
				}
			}
		}
		if len(list) == 0 {
			list = []string{"REJECT"}
			if err == nil {
				warns = append(warns, fmt.Sprintf("前置组「%s」没有可用作前置的节点（带前置代理的节点不能当前置），已填入 REJECT：经由它的节点都连不上，不会改为直连", name))
			}
		}
		ps := make([]any, len(list))
		for i, n := range list {
			ps[i] = n
		}
		groups = append(groups, map[string]any{"name": name, "type": "select", "proxies": ps})
		for _, n := range d.nodes {
			n["dialer-proxy"] = name
		}
		names = append(names, name)
		members = append(members, list)
	}
	return groups, names, members, warns
}

func hasDialer(p map[string]any) bool {
	d, ok := p["dialer-proxy"]
	return ok && d != nil && fmt.Sprint(d) != ""
}

// splitRegexes 编译 mihomo 的 filter / exclude-filter：可以用 ` 分隔多条正则，任意一条匹配即算匹配。
// 有任何一条编译失败返回 ok = false，由调用方按保守的方向处理。
func splitRegexes(v any) (match func(string) bool, ok bool) {
	s, _ := v.(string)
	if strings.TrimSpace(s) == "" {
		return nil, true
	}
	var res []func(string) bool
	for _, part := range strings.Split(s, "`") {
		re, err := compileRegex(part)
		if err != nil {
			return nil, false
		}
		res = append(res, func(n string) bool { return reMatch(re, n) })
	}
	return func(n string) bool {
		for _, m := range res {
			if m(n) {
				return true
			}
		}
		return false
	}, true
}

// groupMembers 按 mihomo 的规则算出每个策略组可能选到的成员（节点名或策略组名）：
//   - proxies 里显式列出的成员
//   - include-all / include-all-proxies 为真时，加上配置里的全部节点（不含策略组）；有 filter 时只取匹配的，
//     再去掉匹配 exclude-filter 的
//
// 为了不漏报，拿不准的都按「可能选到」算：filter 正则无效当作全部匹配，exclude-filter 无效当作不排除；
// exclude-filter 只用在 include-all 引入的节点上，exclude-type 不考虑（类型名和 type 字段的对应关系不可靠）。
// use / include-all-providers 引入的是 proxy-providers 里的节点，不会是 proxies 里的节点，所以不用管。
// byAll[组名][成员] 为真表示这个成员是 include-all 引入的（警告里说明）。
func groupMembers(root map[string]any) (members map[string][]string, byAll map[string]map[string]bool) {
	members, byAll = map[string][]string{}, map[string]map[string]bool{}
	all := namesOf(root["proxies"])
	groups, _ := root["proxy-groups"].([]any)
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		if gm == nil {
			continue
		}
		name := fmt.Sprint(gm["name"])
		seen := map[string]bool{}
		for _, m := range namesOf(gm["proxies"]) {
			if !seen[m] {
				seen[m] = true
				members[name] = append(members[name], m)
			}
		}
		if gm["include-all"] != true && gm["include-all-proxies"] != true {
			continue
		}
		keep, ok := splitRegexes(gm["filter"])
		if !ok {
			keep = nil
		}
		drop, _ := splitRegexes(gm["exclude-filter"])
		for _, n := range all {
			if seen[n] || (keep != nil && !keep(n)) || (drop != nil && drop(n)) {
				continue
			}
			seen[n] = true
			members[name] = append(members[name], n)
			if byAll[name] == nil {
				byAll[name] = map[string]bool{}
			}
			byAll[name][n] = true
		}
	}
	return members, byAll
}

// DialerLoop 是一个前置代理会绕回自己的节点。Path 从节点出发，经过它的前置代理、策略组成员、
// 其他节点的前置代理，回到它自己（首尾都是 Node）。
type DialerLoop struct {
	Node   string   `json:"node"`
	Dialer string   `json:"dialer"`
	Path   []string `json:"path"`
}

// validateDialers 检查节点的前置代理：目标要存在，并且顺着「策略组 → 成员」「节点 → 它的前置代理」走不能回到节点自己，
// 否则客户端选中那条路径时这个节点就连不上。只警告，不改动配置。
func validateDialers(root map[string]any, known map[string]bool) (warns []string, loops []DialerLoop) {
	next, byAll := groupMembers(root) // 名称 → 从它出发可能经过的下一跳
	proxies, _ := root["proxies"].([]any)
	for _, p := range proxies {
		if pm, _ := p.(map[string]any); pm != nil && hasDialer(pm) {
			next[fmt.Sprint(pm["name"])] = []string{fmt.Sprint(pm["dialer-proxy"])}
		}
	}
	// path 找一条 from → … → target 的路径（广度优先，最短），找不到返回 nil
	path := func(from, target string) []string {
		prev := map[string]string{from: ""}
		queue := []string{from}
		for len(queue) > 0 {
			n := queue[0]
			queue = queue[1:]
			if n == target {
				out := []string{}
				for x := n; x != ""; x = prev[x] {
					out = append([]string{x}, out...)
				}
				return out
			}
			for _, m := range next[n] {
				if _, ok := prev[m]; !ok {
					prev[m] = n
					queue = append(queue, m)
				}
			}
		}
		return nil
	}
	missing := map[string]bool{}
	var order []string
	byDialer := map[string][]DialerLoop{}
	for _, p := range proxies {
		pm, _ := p.(map[string]any)
		if pm == nil || !hasDialer(pm) {
			continue
		}
		name, d := fmt.Sprint(pm["name"]), fmt.Sprint(pm["dialer-proxy"])
		if !known[d] {
			if !missing[d] {
				missing[d] = true
				warns = append(warns, fmt.Sprintf("前置代理「%s」不存在（要填策略组名或节点名）", d))
			}
			continue
		}
		if p := path(d, name); p != nil {
			l := DialerLoop{Node: name, Dialer: d, Path: append([]string{name}, p...)}
			loops = append(loops, l)
			if byDialer[d] == nil {
				order = append(order, d)
			}
			byDialer[d] = append(byDialer[d], l)
		}
	}
	for _, d := range order {
		ls := byDialer[d]
		names := []string{}
		for _, l := range ls {
			names = append(names, l.Node)
		}
		more := ""
		if len(names) > 3 {
			more = fmt.Sprintf(" 等 %d 个节点", len(names))
			names = names[:3]
		}
		warns = append(warns, fmt.Sprintf("前置代理「%s」里能选到以它为前置的节点「%s」%s，选中时会形成环、连不上：%s。请让它只包含用作跳板的节点，或改用自动生成的前置组",
			d, strings.Join(names, "」「"), more, describePath(ls[0].Path, byAll)))
	}
	return warns, loops
}

// describePath 把成环的路径写成「A → B（include-all）→ A」，经 include-all 引入的那一步在组名后标出。
func describePath(path []string, byAll map[string]map[string]bool) string {
	parts := make([]string, len(path))
	for i, n := range path {
		parts[i] = n
		if i+1 < len(path) && byAll[n][path[i+1]] {
			parts[i] += "（include-all）"
		}
	}
	return strings.Join(parts, " → ")
}
