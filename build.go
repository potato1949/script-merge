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
	YAML     []byte      `json:"-"`
	Subs     []SubStatus `json:"subs"`
	Warnings []string    `json:"warnings"`
	Total    int         `json:"total"` // 最终节点总数
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
// 基础配置 → 拉取订阅 / 解析手动节点 → 汇总节点 → 策略组 → 规则 → Parser → 校验 → 输出 YAML
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
					entries[i] = &cacheEntry{Proxies: nodes}
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
	add := func(p map[string]any, prefix string) {
		p = deepCopy(p).(map[string]any)
		base := prefix + fmt.Sprint(p["name"])
		name := base
		for n := 2; seen[name]; n++ {
			name = base + " " + strconv.Itoa(n)
		}
		seen[name] = true
		p["name"] = name
		proxies = append(proxies, p)
	}
	var infos []string
	for i, s := range subs {
		e := entries[i]
		if e == nil {
			continue
		}
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
			add(p, s.Prefix)
			res.Subs[i].Used++
		}
	}
	res.Userinfo = mergeUserinfo(infos)
	nodeNames := namesOf(proxies)

	// 3. 策略组
	groups := []any{}
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

	// 4. 规则：前置规则 → 规则集 → 后置规则
	providers := map[string]any{}
	rules := splitLines(p.RulesBefore)
	for _, rs := range p.RuleSets {
		if !rs.Enabled || rs.Name == "" || rs.URL == "" {
			continue
		}
		format := firstNonEmpty(rs.Format, "yaml")
		ext := map[string]string{"yaml": "yaml", "text": "list", "mrs": "mrs"}[format]
		providers[rs.Name] = map[string]any{
			"type": "http", "behavior": firstNonEmpty(rs.Behavior, "classical"), "format": format,
			"url": rs.URL, "path": "./ruleset/" + rs.Name + "." + ext, "interval": 86400,
		}
		rule := "RULE-SET," + rs.Name + "," + firstNonEmpty(rs.Policy, "DIRECT")
		if rs.NoResolve {
			rule += ",no-resolve"
		}
		rules = append(rules, rule)
	}
	rules = append(rules, splitLines(p.RulesAfter)...)
	if rules == nil {
		rules = []any{}
	}

	root["proxies"] = proxies
	root["proxy-groups"] = groups
	if len(providers) > 0 {
		root["rule-providers"] = providers
	}
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

// validate 检查策略组成员和规则引用的策略是否存在。
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
