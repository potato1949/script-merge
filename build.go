package main

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type BuildResult struct {
	YAML     []byte          `json:"-"`
	Subs     []SubStatus     `json:"subs"`
	RuleSets []RuleSetStatus `json:"ruleSets"`
	Warnings []string        `json:"warnings"`
	Total    int             `json:"total"` // 最终节点总数
	Rules    int             `json:"rules"` // 最终规则条数（Parser 之前）
	Userinfo string          `json:"userinfo"`
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

// splitLines 按行拆分规则文本，忽略空行、# 注释和行首的 "- "。
func splitLines(text string) []any {
	var out []any
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimSpace(strings.TrimPrefix(line, "- "))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
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

// resolveUA 决定某个订阅本次拉取上游用哪个 UA：透传模式下用拉取合并链接的客户端的 UA，
// 否则（或客户端没带 UA 时）用全局设置里的固定 UA。
func resolveUA(cfg Config, s Source, clientUA string) string {
	if s.UAMode == "passthrough" && clientUA != "" {
		return clientUA
	}
	return firstNonEmpty(strings.TrimSpace(cfg.UserAgent), defaultUA)
}

// Build 为订阅组 p 拉取订阅并生成最终配置。clientUA 是拉取合并链接的客户端的 UA。流程：
// 基础配置 → 拉取订阅 / 解析手动节点 → 汇总节点（带上节点地址解析要用的 hosts）→ 策略组 → 规则（规则集展开成平铺规则）
// → Parser → 校验 → 输出 YAML
func Build(cfg Config, p Profile, f *Fetcher, clientUA string, force bool) (*BuildResult, error) {
	res := &BuildResult{Warnings: []string{}}
	warn := func(format string, a ...any) { res.Warnings = append(res.Warnings, fmt.Sprintf(format, a...)) }

	root := map[string]any{}
	if err := yaml.Unmarshal([]byte(cfg.BaseConfig), &root); err != nil {
		return nil, fmt.Errorf("基础配置 YAML 解析失败: %w", err)
	}

	// 1. 并发拉取订阅
	// 来源是全局对象，订阅组里只有引用；同一个来源被多个订阅组（或同组多次）引用时共用缓存
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
		if !ref.Enabled {
			continue
		}
		src, ok := byID[ref.Source]
		if !ok { // validateConfig 会去掉悬空引用，正常不会走到
			warn("第 %d 个节点来源不存在，已忽略", i+1)
			continue
		}
		subs = append(subs, usedSource{src, ref, i})
	}
	entries := make([]*cacheEntry, len(subs))
	res.Subs = make([]SubStatus, len(subs))
	ttl := time.Duration(cfg.CacheMinutes) * time.Minute
	unlockUA := firstNonEmpty(strings.TrimSpace(cfg.UnlockUserAgent), chromeUA)
	var wg sync.WaitGroup
	for i, s := range subs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.Kind == "manual" {
				st := SubStatus{Index: s.index, ID: s.ID, Name: s.Name}
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
			url, ua := strings.TrimSpace(s.URL), resolveUA(cfg, s.Source, clientUA)
			st := SubStatus{Index: s.index, ID: s.ID, Name: s.Name, URL: url, UA: ua}
			if url == "" {
				st.Error = "没有填写订阅链接"
				res.Subs[i] = st
				return
			}
			e, err := f.Get(s.Source, url, ua, unlockUA, ttl, force)
			if err != nil {
				st.Error = err.Error()
				st.Stale = e != nil
			}
			if e != nil {
				st.Count = len(e.Proxies)
				st.FetchedAt = e.FetchedAt
			}
			entries[i], res.Subs[i] = e, st
		}()
	}
	wg.Wait()

	// 2. 汇总节点（重名节点自动加序号）
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
	renames := make([]map[string][]string, len(subs)) // 每条引用：上游节点名 → 输出里的名字（被过滤掉的没有）
	for i, s := range subs {
		e := entries[i]
		if e == nil {
			continue
		}
		renames[i] = map[string][]string{}
		var servers []string
		if e.Userinfo != "" {
			infos = append(infos, e.Userinfo)
		}
		inc, err := optRegex(s.Include)
		if err != nil {
			warn("订阅「%s」包含%v", s.Name, err)
		}
		exc, err := optRegex(s.Exclude)
		if err != nil {
			warn("订阅「%s」排除%v", s.Name, err)
		}
		for _, p := range e.Proxies {
			name := fmt.Sprint(p["name"])
			if (inc != nil && !reMatch(inc, name)) || (exc != nil && reMatch(exc, name)) {
				continue
			}
			renames[i][name] = append(renames[i][name], add(p, s.Prefix, strings.TrimSpace(s.Dialer)))
			if server, ok := p["server"].(string); ok {
				servers = append(servers, server)
			}
			res.Subs[i].Used++
		}
		hosts, filter := nodeHosts(&e.upstream, servers)
		res.Subs[i].Hosts = len(hosts)
		res.Warnings = append(res.Warnings, mergeNodeHosts(root, hosts, filter)...)
	}
	res.Userinfo = mergeUserinfo(infos)
	nodeNames := namesOf(proxies)

	// 3. 策略组：先是沿用的机场策略组，再是本组自己的；重名时本组自己的优先
	groups := []any{}
	own, taken := map[string]bool{}, map[string]bool{}
	for _, g := range p.Groups {
		own[strings.TrimSpace(g.Name)] = true
	}
	for i, s := range subs {
		if !s.Follow || entries[i] == nil {
			continue
		}
		if len(entries[i].Groups) == 0 && len(entries[i].Rules) == 0 {
			warn("「%s」选了沿用机场的策略组和规则，但上游内容里没有（只有 Clash 格式的订阅才带）", s.Name)
			continue
		}
		gs, ws := followGroups(s.Name, &entries[i].upstream, renames[i], nodeNames)
		res.Warnings = append(res.Warnings, ws...)
		for _, g := range gs {
			name := fmt.Sprint(g["name"])
			if own[name] || taken[name] {
				warn("「%s」的策略组「%s」和已有的策略组重名，没有带过来", s.Name, name)
				continue
			}
			taken[name] = true
			groups = append(groups, g)
		}
	}
	for _, g := range p.Groups {
		if strings.TrimSpace(g.Name) == "" {
			continue
		}
		members := []any{}
		for _, e := range g.Extra {
			if e = strings.TrimSpace(e); e != "" {
				members = append(members, e)
			}
		}
		switch g.Nodes {
		case "all":
			for _, n := range nodeNames {
				members = append(members, n)
			}
		case "filter":
			re, err := optRegex(g.Filter)
			if err != nil {
				warn("策略组「%s」筛选%v", g.Name, err)
			}
			for _, n := range nodeNames {
				if re != nil && reMatch(re, n) {
					members = append(members, n)
				}
			}
		}
		if len(members) == 0 {
			warn("策略组「%s」没有任何成员，已填入 DIRECT", g.Name)
			members = append(members, "DIRECT")
		}
		typ := firstNonEmpty(g.Type, "select")
		gm := map[string]any{"name": g.Name, "type": typ, "proxies": members}
		if typ != "select" {
			gm["url"] = firstNonEmpty(g.TestURL, "https://www.gstatic.com/generate_204")
			gm["interval"] = 300
			if g.Interval > 0 {
				gm["interval"] = g.Interval
			}
		}
		groups = append(groups, gm)
	}

	// 4. 规则：前置规则 → 规则集 → 沿用的机场规则 → 后置规则。
	// 规则集和机场规则里的 RULE-SET 都由服务端下载规则文件，展开成平铺的规则；排在后面的重复规则去掉
	var follows []followSource
	for i, s := range subs {
		if s.Follow && entries[i] != nil {
			follows = append(follows, followSource{s.Name, &entries[i].upstream})
		}
	}
	rules := buildRules(p, follows, f.rules, res)
	if rules == nil {
		rules = []any{}
	}
	res.Rules = len(rules)
	if res.Rules > ruleWarnTotal {
		warn("规则共 %d 条。平铺的规则客户端要逐条匹配，太多会变慢，可以换用更精简的规则集", res.Rules)
	}

	root["proxies"] = proxies
	root["proxy-groups"] = groups
	root["rules"] = rules

	// 5. CFW Parser
	pw, err := ApplyParser(root, p.Parser)
	if err != nil {
		return nil, err
	}
	res.Warnings = append(res.Warnings, pw...)
	res.Warnings = append(res.Warnings, validate(root)...)
	res.Total = len(namesOf(root["proxies"]))

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(toNode(root, 0, "")); err != nil {
		return nil, err
	}
	res.YAML = unescapeRunes(buf.Bytes())
	return res, nil
}

// followSource 是一条选了「沿用机场的策略组和规则」的来源引用拉到的上游内容。
type followSource struct {
	name string
	up   *upstream
}

// buildRules 生成最终的规则列表：前置规则 → 规则集 → 沿用的机场规则 → 后置规则。
// 规则集和机场规则里的 RULE-SET 引用的规则文件一起并发下载，展开成带策略的规则；排在后面的重复规则去掉。
func buildRules(p Profile, follows []followSource, rf *RuleFetcher, res *BuildResult) []any {
	warn := func(format string, a ...any) { res.Warnings = append(res.Warnings, fmt.Sprintf(format, a...)) }
	var l ruleList
	for _, r := range splitLines(p.RulesBefore) {
		l.add(fmt.Sprint(r))
	}

	// 先收集要下载的地址，一次并发下载
	var urls []string
	idx := map[string]int{}
	need := func(u string) {
		if _, ok := idx[u]; !ok {
			idx[u] = len(urls)
			urls = append(urls, u)
		}
	}
	var sets []RuleSet
	for _, rs := range p.RuleSets {
		if rs.URL = strings.TrimSpace(rs.URL); rs.Enabled && rs.URL != "" {
			sets = append(sets, rs)
			need(rs.URL)
		}
	}
	followSets := make([]map[int]followRuleSet, len(follows)) // 机场规则的下标 → 它引用的规则文件
	for i, fs := range follows {
		followSets[i] = map[int]followRuleSet{}
		for j, r := range fs.up.Rules {
			if typ, _, _ := strings.Cut(r, ","); strings.TrimSpace(typ) == "RULE-SET" {
				rs := parseFollowRuleSet(fs.up, r)
				if rs.url != "" {
					need(rs.url)
				}
				followSets[i][j] = rs
			}
		}
	}
	entries, errs := rf.fetchAll(urls)

	// expand 把一个规则文件按策略展开，返回预览里显示的状态
	expand := func(name, url, policy string) RuleSetStatus {
		st := RuleSetStatus{Name: name, URL: url}
		e, err := entries[idx[url]], errs[idx[url]]
		if err != nil {
			st.Error, st.Stale = err.Error(), e != nil
			if e == nil {
				warn("规则集「%s」下载失败，已跳过：%v", name, err)
			} else {
				warn("规则集「%s」下载失败，用了 %s 的缓存：%v", name, e.FetchedAt.Format("01-02 15:04"), err)
			}
		}
		if e != nil {
			st.Count, st.Skipped, st.FetchedAt = len(e.Rules), e.Skipped, e.FetchedAt
			for _, r := range e.Rules {
				l.add(withPolicy(r, policy))
			}
		}
		return st
	}
	for _, rs := range sets {
		res.RuleSets = append(res.RuleSets, expand(firstNonEmpty(strings.TrimSpace(rs.Name), rs.URL), rs.URL, firstNonEmpty(strings.TrimSpace(rs.Policy), "DIRECT")))
	}

	matched := "" // 以 MATCH 结尾的机场规则来自哪个来源
	for i, fs := range follows {
		for j, r := range fs.up.Rules {
			typ, _, _ := strings.Cut(r, ",")
			switch strings.TrimSpace(typ) {
			case "RULE-SET":
				rs := followSets[i][j]
				switch {
				case rs.err != "":
					warn("「%s」的规则「%s」没有展开：%s", fs.name, r, rs.err)
				case rs.url != "":
					res.RuleSets = append(res.RuleSets, expand(fs.name+" · "+rs.name, rs.url, rs.policy))
				default:
					res.RuleSets = append(res.RuleSets, RuleSetStatus{Name: fs.name + " · " + rs.name, Count: len(rs.inline)})
					for _, x := range rs.inline {
						l.add(withPolicy(x, rs.policy))
					}
				}
				continue
			case "SUB-RULE":
				warn("「%s」的规则「%s」用到了子规则，没有带过来", fs.name, r)
				continue
			case "MATCH":
				matched = fs.name
			}
			l.add(r)
		}
	}
	after := splitLines(p.RulesAfter)
	if matched != "" && len(after) > 0 {
		warn("沿用的「%s」的规则以 MATCH 结尾，后置规则不会生效", matched)
	}
	for _, r := range after {
		l.add(fmt.Sprint(r))
	}
	return l.rules
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
		case strings.ContainsAny(parts[0], "()") || parts[0] == "AND" || parts[0] == "OR" || parts[0] == "NOT":
			continue // 逻辑规则不做校验
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
