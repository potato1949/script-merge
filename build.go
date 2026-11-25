package main

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type BuildResult struct {
	YAML     []byte      `json:"-"`
	Subs     []SubStatus `json:"subs"`
	Warnings []string    `json:"warnings"`
	Total    int         `json:"total"` // 最终节点总数
	Groups   int         `json:"groups"`
	Rules    int         `json:"rules"`
	Userinfo string      `json:"userinfo"`
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[k] = deepCopy(x)
		}
		return m
	case []any:
		l := make([]any, len(t))
		for i, x := range t {
			l[i] = deepCopy(x)
		}
		return l
	}
	return v
}

// parseCustomProxies 解析手动节点：接受 YAML 数组、带 proxies: 的 YAML，或每行一条分享链接。
func parseCustomProxies(text string) ([]map[string]any, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	var v any
	if err := yaml.Unmarshal([]byte(text), &v); err == nil {
		if m, ok := v.(map[string]any); ok {
			v = m["proxies"]
		}
		if list, ok := v.([]any); ok {
			var out []map[string]any
			for i, it := range list {
				p, ok := it.(map[string]any)
				if name, _ := p["name"].(string); !ok || name == "" {
					return nil, fmt.Errorf("自定义节点第 %d 项缺少 name", i+1)
				}
				out = append(out, p)
			}
			return out, nil
		}
	}
	if out := parseURIList(text); len(out) > 0 {
		return out, nil
	}
	return nil, errors.New("自定义节点格式无法识别（需要 YAML 数组或分享链接）")
}

// errNoCache：只用缓存构建时，底板还没有可用的缓存。
var errNoCache = errors.New("底板机场还没有缓存")

// Build 为订阅组 p 生成最终配置。流程：
// 并发拉取本组引用的来源 → 以底板机场的完整配置为基础 → 汇总节点（底板的在前；过滤、加前缀、重名加序号）
// → 底板的策略组换上新的节点名，包含底板全部节点的组再加上其他来源的节点 → 带上其他来源节点地址要用的 hosts
// → Parser → 校验 → 输出 YAML。底板的规则、rule-providers、DNS 等其余内容原样保留。
func Build(cfg Config, p Profile, f *Fetcher, mode FetchMode) (*BuildResult, error) {
	res := &BuildResult{Warnings: []string{}}
	warn := func(format string, a ...any) { res.Warnings = append(res.Warnings, fmt.Sprintf(format, a...)) }

	// 1. 本组用到的来源，底板排第一个。来源是全局对象，订阅组里只有引用；
	// 同一个来源被多个订阅组（或同组多次）引用时共用缓存
	type usedSource struct {
		Source
		SourceRef
		index int
	}
	byID := map[string]Source{}
	for _, s := range cfg.Sources {
		byID[s.ID] = s
	}
	var subs []usedSource
	for i, ref := range p.Sources {
		src, ok := byID[ref.Source]
		if !ok { // validateConfig 会去掉悬空引用，正常不会走到
			warn("第 %d 个节点来源不存在，已忽略", i+1)
			continue
		}
		switch {
		case ref.Source == p.Base && (len(subs) == 0 || subs[0].ID != p.Base): // 底板始终使用，不看启用开关
			subs = append([]usedSource{{src, ref, i}}, subs...)
		case ref.Enabled:
			subs = append(subs, usedSource{src, ref, i})
		}
	}
	if base, ok := byID[p.Base]; !ok || base.Kind == "manual" || len(subs) == 0 || subs[0].ID != p.Base {
		return nil, errors.New("还没有选择底板机场：在本组的节点来源里把一个远程订阅设为底板")
	}

	// 2. 并发拉取
	entries := make([]*cacheEntry, len(subs))
	res.Subs = make([]SubStatus, len(subs))
	ttl := time.Duration(cfg.CacheMinutes) * time.Minute
	var wg sync.WaitGroup
	for i, s := range subs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := SubStatus{Index: s.index, ID: s.ID, Name: s.Name, Base: i == 0}
			if s.Kind == "manual" {
				nodes, err := parseCustomProxies(s.Nodes)
				if err != nil {
					st.Error = err.Error()
				} else {
					entries[i] = &cacheEntry{upstream: upstream{Proxies: nodes}}
					st.Count = len(nodes)
				}
				res.Subs[i] = st
				return
			}
			url, ua, unlockUA := fetchArgs(cfg, s.Source)
			if url == "" {
				st.Error = "没有填写订阅链接"
				res.Subs[i] = st
				return
			}
			e, err := f.Get(s.Source, url, ua, unlockUA, ttl, mode)
			if err != nil {
				st.Error = err.Error()
				st.Stale = e != nil
			}
			if e != nil {
				st.Count = len(e.Proxies)
			} else if mode == FetchCacheOnly {
				st.NoCache = true
			}
			entries[i], res.Subs[i] = e, st
		}()
	}
	wg.Wait()

	base := entries[0]
	switch {
	case base == nil && mode == FetchCacheOnly:
		return nil, errNoCache
	case base == nil:
		return nil, fmt.Errorf("底板机场「%s」拉取失败，也没有可用的缓存：%s", subs[0].Name, res.Subs[0].Error)
	case len(base.groups()) == 0:
		return nil, fmt.Errorf("底板机场「%s」返回的不是带策略组的 Clash 配置，不能作为底板。可以在全局设置里换一个 UA（如 clash.meta）再试，或者换一个机场做底板", subs[0].Name)
	}
	root := deepCopy(base.Doc).(map[string]any)

	// 3. 汇总节点（重名节点自动加序号）
	proxies := []any{}
	seen := map[string]bool{}
	add := func(p map[string]any, prefix, dialer string) string {
		p = deepCopy(p).(map[string]any)
		if dialer != "" { // 前置代理：覆盖节点自带的 dialer-proxy
			p["dialer-proxy"] = dialer
		}
		base := prefix + fmt.Sprint(p["name"])
		name := base
		for n := 2; seen[name]; n++ {
			name = base + " " + strconv.Itoa(n)
		}
		seen[name] = true
		p["name"] = name
		proxies = append(proxies, p)
		return name
	}
	var infos []string
	infoFrom := map[string]bool{}   // 同一个来源被引用多次时，流量信息只算一次
	rename := map[string][]string{} // 底板：上游节点名 → 输出里的名字（被过滤掉的没有）
	var others []string             // 其他来源的节点在输出里的名字
	for i, s := range subs {
		e := entries[i]
		if e == nil {
			continue
		}
		if e.Userinfo != "" && !infoFrom[s.ID] {
			infoFrom[s.ID] = true
			infos = append(infos, e.Userinfo)
		}
		inc, err := optRegex(s.Include)
		if err != nil {
			warn("「%s」的只保留%v", s.Name, err)
		}
		exc, err := optRegex(s.Exclude)
		if err != nil {
			warn("「%s」的排除%v", s.Name, err)
		}
		var servers []string
		res.Subs[i].Nodes = make([]NodeChange, 0, len(e.Proxies))
		for _, p := range e.Proxies {
			name := fmt.Sprint(p["name"])
			if (inc != nil && !reMatch(inc, name)) || (exc != nil && reMatch(exc, name)) {
				res.Subs[i].Nodes = append(res.Subs[i].Nodes, NodeChange{Name: name})
				continue
			}
			out := add(p, s.Prefix, strings.TrimSpace(s.Dialer))
			res.Subs[i].Nodes = append(res.Subs[i].Nodes, NodeChange{Name: name, Out: out})
			if i == 0 {
				rename[name] = append(rename[name], out)
			} else {
				others = append(others, out)
			}
			if server, ok := p["server"].(string); ok {
				servers = append(servers, server)
			}
			res.Subs[i].Used++
		}
		if i > 0 { // 底板自己的 hosts 已经整份在输出里了
			hosts, filter := nodeHosts(&e.upstream, servers)
			res.Subs[i].Hosts = len(hosts)
			res.Warnings = append(res.Warnings, mergeNodeHosts(root, hosts, filter)...)
		}
	}
	res.Userinfo = mergeUserinfo(infos)

	// 4. 策略组：底板的，换上新的节点名
	groups, ws := baseGroups(&base.upstream, rename, others)
	res.Warnings = append(res.Warnings, ws...)
	root["proxies"] = proxies
	root["proxy-groups"] = groups
	if len(others) > 0 && !slices.ContainsFunc(groups, func(g any) bool {
		gm := g.(map[string]any)
		ps, _ := gm["proxies"].([]any)
		return slices.Contains(ps, any(others[0])) || gm["include-all"] == true || gm["include-all-proxies"] == true
	}) {
		warn("底板里没有包含它全部节点的策略组，其他来源的节点没有加进任何策略组。可以用 Parser 把它们加到需要的策略组里")
	}

	// 5. CFW Parser
	pw, err := ApplyParser(root, p.Parser)
	if err != nil {
		return nil, err
	}
	res.Warnings = append(res.Warnings, pw...)
	res.Warnings = append(res.Warnings, validate(root)...)
	res.Total = len(namesOf(root["proxies"]))
	res.Groups = len(namesOf(root["proxy-groups"]))
	rules, _ := root["rules"].([]any)
	res.Rules = len(rules)

	if res.YAML, err = encodeYAML(root); err != nil {
		return nil, err
	}
	return res, nil
}

// encodeYAML 把配置树按输出的键顺序编码成 YAML（节点每个一行）。
func encodeYAML(root map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(toNode(root, 0, "")); err != nil {
		return nil, err
	}
	return unescapeRunes(buf.Bytes()), nil
}

var escapedRune = regexp.MustCompile(`\\\\|\\U[0-9A-F]{8}`)

// unescapeRunes 把 yaml.v3 对 emoji 等字符输出的 \UXXXXXXXX 转义还原为原字符（`\\` 保持不动）。
func unescapeRunes(b []byte) []byte {
	return escapedRune.ReplaceAllFunc(b, func(m []byte) []byte {
		if m[1] != 'U' {
			return m
		}
		n, _ := strconv.ParseUint(string(m[2:]), 16, 32)
		return []byte(string(rune(n)))
	})
}

var builtinPolicies = map[string]bool{"DIRECT": true, "REJECT": true, "REJECT-DROP": true, "PASS": true, "COMPATIBLE": true, "GLOBAL": true}

// validate 检查策略组成员、规则引用的策略、节点的前置代理（dialer-proxy）是否存在，以及前置代理有没有绕回节点自己。
func validate(root map[string]any) (warns []string) {
	known := map[string]bool{}
	for _, n := range namesOf(root["proxies"]) {
		known[n] = true
	}
	groups, _ := root["proxy-groups"].([]any)
	for _, n := range namesOf(groups) {
		known[n] = true
	}
	ok := func(n string) bool { return known[n] || builtinPolicies[n] }
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		members, _ := gm["proxies"].([]any)
		for _, m := range members {
			if !ok(fmt.Sprint(m)) {
				warns = append(warns, fmt.Sprintf("策略组「%v」引用了不存在的成员「%v」", gm["name"], m))
			}
		}
	}
	warns = append(warns, validateDialers(root, known)...)
	missing := map[string]bool{}
	rules, _ := root["rules"].([]any)
	for _, r := range rules {
		parts := strings.Split(fmt.Sprint(r), ",")
		var policy string
		switch {
		case parts[0] == "MATCH" && len(parts) >= 2:
			policy = parts[1]
		case strings.ContainsAny(parts[0], "()") || parts[0] == "AND" || parts[0] == "OR" || parts[0] == "NOT" || parts[0] == "SUB-RULE":
			continue // 逻辑规则、子规则不做校验
		case len(parts) >= 3:
			policy = parts[2]
		default:
			continue
		}
		if policy = strings.TrimSpace(policy); !ok(policy) && !missing[policy] {
			missing[policy] = true
			warns = append(warns, fmt.Sprintf("规则引用了不存在的策略「%s」", policy))
		}
	}
	return warns
}

// validateDialers 检查节点的前置代理：目标要存在，并且顺着「策略组 → 成员」「节点 → 它的前置代理」走不能回到节点自己，
// 否则客户端选中那条路径时这个节点就连不上。只警告，不改动配置。
func validateDialers(root map[string]any, known map[string]bool) (warns []string) {
	next := map[string][]string{} // 名称 → 从它出发可能经过的下一跳
	proxies, _ := root["proxies"].([]any)
	for _, p := range proxies {
		if pm, _ := p.(map[string]any); pm != nil {
			if d, ok := pm["dialer-proxy"]; ok && fmt.Sprint(d) != "" {
				next[fmt.Sprint(pm["name"])] = []string{fmt.Sprint(d)}
			}
		}
	}
	groups, _ := root["proxy-groups"].([]any)
	for _, g := range groups {
		if gm, _ := g.(map[string]any); gm != nil {
			members, _ := gm["proxies"].([]any)
			for _, m := range members {
				next[fmt.Sprint(gm["name"])] = append(next[fmt.Sprint(gm["name"])], fmt.Sprint(m))
			}
		}
	}
	reaches := func(from, target string) bool {
		seen := map[string]bool{}
		stack := []string{from}
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if n == target {
				return true
			}
			if seen[n] {
				continue
			}
			seen[n] = true
			stack = append(stack, next[n]...)
		}
		return false
	}
	missing := map[string]bool{}
	var looped []string
	loopedBy := map[string][]string{} // 前置代理 → 会绕回自己的节点
	for _, name := range namesOf(proxies) {
		ds := next[name]
		if len(ds) == 0 {
			continue
		}
		d := ds[0]
		switch {
		case !known[d]:
			if !missing[d] {
				missing[d] = true
				warns = append(warns, fmt.Sprintf("前置代理「%s」不存在（要填策略组名或节点名）", d))
			}
		case reaches(d, name):
			if loopedBy[d] == nil {
				looped = append(looped, d)
			}
			loopedBy[d] = append(loopedBy[d], name)
		}
	}
	for _, d := range looped {
		names := loopedBy[d]
		more := ""
		if len(names) > 3 {
			more = fmt.Sprintf(" 等 %d 个节点", len(names))
			names = names[:3]
		}
		warns = append(warns, fmt.Sprintf("前置代理「%s」里能选到以它为前置的节点「%s」%s，选中时会形成环、连不上；请让它只包含用作跳板的节点", d, strings.Join(names, "」「"), more))
	}
	return warns
}

// mergeUserinfo 汇总多个订阅的流量信息：流量求和，到期时间取最早。
func mergeUserinfo(infos []string) string {
	if len(infos) == 0 {
		return ""
	}
	sum := map[string]int64{}
	var expire int64
	for _, info := range infos {
		for _, kv := range strings.Split(info, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
			if !ok {
				continue
			}
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				continue
			}
			if k = strings.ToLower(strings.TrimSpace(k)); k == "expire" {
				if n > 0 && (expire == 0 || n < expire) {
					expire = n
				}
			} else {
				sum[k] += n
			}
		}
	}
	out := fmt.Sprintf("upload=%d; download=%d; total=%d", sum["upload"], sum["download"], sum["total"])
	if expire > 0 {
		out += fmt.Sprintf("; expire=%d", expire)
	}
	return out
}

var (
	headKeys = []string{"port", "socks-port", "mixed-port", "redir-port", "tproxy-port", "allow-lan", "bind-address", "mode", "log-level", "ipv6", "external-controller", "secret"}
	tailKeys = []string{"proxies", "proxy-groups", "proxy-providers", "rule-providers", "rules"}
	itemKeys = []string{"name", "type", "server", "port"}
)

func orderKeys(m map[string]any, depth int) []string {
	rank := map[string]int{}
	if depth == 0 {
		for i, k := range headKeys {
			rank[k] = i - len(headKeys)
		}
		for i, k := range tailKeys {
			rank[k] = i + 1
		}
	} else {
		for i, k := range itemKeys {
			rank[k] = i - len(itemKeys)
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ri, rj := rank[keys[i]], rank[keys[j]]
		if ri != rj {
			return ri < rj
		}
		return keys[i] < keys[j]
	})
	return keys
}

// toNode 把数据转换为键顺序稳定的 YAML 节点；proxies 的每个节点输出为单行。
func toNode(v any, depth int, parent string) *yaml.Node {
	switch t := v.(type) {
	case map[string]any:
		n := &yaml.Node{Kind: yaml.MappingNode}
		for _, k := range orderKeys(t, depth) {
			key := &yaml.Node{}
			_ = key.Encode(k)
			n.Content = append(n.Content, key, toNode(t[k], depth+1, k))
		}
		return n
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode}
		for _, it := range t {
			child := toNode(it, depth+1, "")
			if depth == 1 && parent == "proxies" && child.Kind == yaml.MappingNode {
				child.Style = yaml.FlowStyle
			}
			n.Content = append(n.Content, child)
		}
		return n
	case float64:
		if t == float64(int64(t)) {
			v = int64(t)
		}
	}
	n := &yaml.Node{}
	_ = n.Encode(v)
	return n
}
