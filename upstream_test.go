package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParseRuleFile(t *testing.T) {
	// YAML：classical / domain / ipcidr 混在一起也能逐行认出
	rules, skipped, err := parseRuleFile([]byte(`# 注释
payload:
  - DOMAIN-SUFFIX,a.com
  - 'IP-CIDR,1.0.0.0/8,no-resolve'
  - '+.b.com'
  - .c.com
  - d.com
  - '*.e.com'
  - 91.108.4.0/22
  - 2001:b28:f23c::/47
  - 8.8.8.8
  - PROCESS-NAME,frpc.exe
  - AND,((DOMAIN,x.com),(NETWORK,UDP))
  - USER-AGENT,foo*
  - IP-CIDR,not-a-cidr
  - DOMAIN-REGEX,(unclosed
  - has space
`))
	want := []string{
		"DOMAIN-SUFFIX,a.com", "IP-CIDR,1.0.0.0/8,no-resolve", "DOMAIN-SUFFIX,b.com", "DOMAIN-SUFFIX,c.com", "DOMAIN,d.com",
		`DOMAIN-REGEX,^[^.]+\.e\.com$`, "IP-CIDR,91.108.4.0/22", "IP-CIDR6,2001:b28:f23c::/47", "IP-CIDR,8.8.8.8/32",
		"PROCESS-NAME,frpc.exe", "AND,((DOMAIN,x.com),(NETWORK,UDP))",
	}
	if err != nil || !reflect.DeepEqual(rules, want) || skipped != 4 {
		t.Errorf("YAML: err=%v skipped=%d\n got %q\nwant %q", err, skipped, rules, want)
	}

	// 文本：一行一条，# 和 // 是注释
	rules, _, err = parseRuleFile([]byte("# Telegram\n//x\nDOMAIN-SUFFIX,t.me\n\nIP-CIDR,91.108.0.0/16,no-resolve\n+.x.com\n"))
	if err != nil || !reflect.DeepEqual(rules, []string{"DOMAIN-SUFFIX,t.me", "IP-CIDR,91.108.0.0/16,no-resolve", "DOMAIN-SUFFIX,x.com"}) {
		t.Errorf("文本: %q %v", rules, err)
	}

	// 二进制（mrs 是 zstd 压缩的）和空文件都报错
	if _, _, err := parseRuleFile([]byte{0x28, 0xb5, 0x2f, 0xfd, 1, 2}); err == nil {
		t.Error("mrs 应当报错")
	}
	if _, _, err := parseRuleFile([]byte("payload: []\n")); err == nil {
		t.Error("没有规则应当报错")
	}
}

func TestWithPolicy(t *testing.T) {
	for in, want := range map[string]string{
		"DOMAIN-SUFFIX,a.com":            "DOMAIN-SUFFIX,a.com,P",
		"IP-CIDR,1.0.0.0/8":              "IP-CIDR,1.0.0.0/8,P,no-resolve", // IP 规则一律加 no-resolve
		"IP-CIDR,1.0.0.0/8,no-resolve":   "IP-CIDR,1.0.0.0/8,P,no-resolve",
		"IP-ASN,13414":                   "IP-ASN,13414,P,no-resolve",
		"SRC-IP-CIDR,1.0.0.0/8":          "SRC-IP-CIDR,1.0.0.0/8,P", // 按来源 IP 的不需要
		"AND,((DOMAIN,x),(NETWORK,UDP))": "AND,((DOMAIN,x),(NETWORK,UDP)),P",
	} {
		if got := withPolicy(in, "P"); got != want {
			t.Errorf("withPolicy(%q) = %q，应为 %q", in, got, want)
		}
	}
	var l ruleList
	for _, r := range []string{"DOMAIN,a.com,X", "DOMAIN,a.com,Y", "IP-CIDR,1.0.0.0/8,X,no-resolve", "IP-CIDR,1.0.0.0/8,Y", "AND,((DOMAIN,x)),X", "AND,((DOMAIN,x)),Y"} {
		l.add(r)
	}
	if want := []any{"DOMAIN,a.com,X", "IP-CIDR,1.0.0.0/8,X,no-resolve", "AND,((DOMAIN,x)),X"}; !reflect.DeepEqual(l.rules, want) {
		t.Errorf("去重: %v", l.rules)
	}
}

func TestRuleFetcherCache(t *testing.T) {
	old := ruleDownload
	defer func() { ruleDownload = old }()
	hits, fail := 0, false
	ruleDownload = func(string) ([]byte, error) {
		hits++
		if fail {
			return nil, fmt.Errorf("断网")
		}
		return []byte("a.com\n"), nil
	}
	dir := t.TempDir()
	const u = "https://rules.example/a.list"
	if e, err := NewRuleFetcher(dir).Get(u); err != nil || !reflect.DeepEqual(e.Rules, []string{"DOMAIN,a.com"}) {
		t.Fatalf("%+v %v", e, err)
	}
	// 新的实例从磁盘读缓存，一天内不重新下载
	f := NewRuleFetcher(dir)
	if _, err := f.Get(u); err != nil || hits != 1 {
		t.Errorf("应当用磁盘缓存: hits=%d err=%v", hits, err)
	}
	// 过期后下载失败：返回旧缓存和错误
	f.cache[u].FetchedAt = f.cache[u].FetchedAt.Add(-2 * ruleTTL)
	fail = true
	if e, err := f.Get(u); err == nil || e == nil || len(e.Rules) != 1 {
		t.Errorf("失败时应返回旧缓存: %+v %v", e, err)
	}
	if _, err := NewRuleFetcher(dir).Get("https://rules.example/x.mrs"); err == nil || !strings.Contains(err.Error(), "mrs") {
		t.Errorf("mrs 地址应当报错: %v", err)
	}
}

func TestNodeHosts(t *testing.T) {
	up := &upstream{
		Hosts: map[string]any{
			"n1.node.sbs":  "n2.node.sbs", // 两层映射
			"n2.node.sbs":  []any{"1.2.3.4"},
			"+.other.sbs":  "x.sbs", // 通配：节点地址 a.other.sbs 匹配
			"unrelated.io": "5.6.7.8",
		},
		FakeIPFilter: []string{"*.lan", "n1.node.sbs", "n2.node.sbs", "+.other.sbs", "geosite:cn"},
	}
	hosts, filter := nodeHosts(up, []string{"N1.node.sbs", "a.other.sbs", "9.9.9.9"})
	if want := map[string]any{"n1.node.sbs": "n2.node.sbs", "n2.node.sbs": []any{"1.2.3.4"}, "+.other.sbs": "x.sbs"}; !reflect.DeepEqual(hosts, want) {
		t.Errorf("hosts = %v", hosts)
	}
	if want := []string{"n1.node.sbs", "n2.node.sbs", "+.other.sbs"}; !reflect.DeepEqual(filter, want) {
		t.Errorf("fake-ip-filter = %v", filter)
	}

	root := map[string]any{"hosts": map[string]any{"n1.node.sbs": "别的"}, "dns": map[string]any{"fake-ip-filter": []any{"*.lan"}}}
	warns := mergeNodeHosts(root, hosts, filter)
	if len(warns) != 1 || root["hosts"].(map[string]any)["n1.node.sbs"] != "别的" || root["hosts"].(map[string]any)["n2.node.sbs"] == nil {
		t.Errorf("基础配置里已有的 hosts 应当保留并提醒: %v %v", root["hosts"], warns)
	}
	if got := root["dns"].(map[string]any)["fake-ip-filter"]; !reflect.DeepEqual(got, []any{"*.lan", "n1.node.sbs", "n2.node.sbs", "+.other.sbs"}) {
		t.Errorf("fake-ip-filter 合并结果 = %v", got)
	}
	white := map[string]any{"dns": map[string]any{"fake-ip-filter-mode": "whitelist"}}
	mergeNodeHosts(white, nil, filter)
	if white["dns"].(map[string]any)["fake-ip-filter"] != nil {
		t.Errorf("白名单模式下不应追加")
	}
}

// 沿用机场的策略组和规则：节点名换成改名后的，「全部节点」的组带上其他来源的节点，RULE-SET 展开，hosts 带过来
func TestFollowUpstream(t *testing.T) {
	airport := `proxies:
  - {name: 香港 01, type: ss, server: n1.node.sbs, port: 443, cipher: aes-128-gcm, password: x}
  - {name: 日本 01, type: ss, server: 1.1.1.1, port: 443, cipher: aes-128-gcm, password: x}
  - {name: 剩余流量, type: ss, server: 1.1.1.2, port: 443, cipher: aes-128-gcm, password: x}
proxy-groups:
  - {name: Proxies, type: select, proxies: [Auto, 香港 01, 日本 01, 剩余流量, DIRECT]}
  - {name: Auto, type: url-test, url: 'http://t', interval: 300, proxies: [香港 01, 日本 01, 剩余流量]}
  - {name: 香港, type: select, proxies: [香港 01]}
  - {name: 过期, type: select, proxies: [剩余流量]}
  - {name: 我的, type: select, proxies: [DIRECT]}
rule-providers:
  remote: {type: http, behavior: domain, url: 'https://rules.example/remote.yaml', path: ./r.yaml, interval: 86400}
  inl: {type: inline, behavior: ipcidr, payload: ['10.0.0.0/8']}
  gone: {type: file, behavior: domain, path: ./x.yaml}
rules:
  - DOMAIN,mine.com,Proxies
  - RULE-SET,remote,Proxies
  - RULE-SET,inl,DIRECT,no-resolve
  - RULE-SET,gone,DIRECT
  - DOMAIN-SUFFIX,remote.example,DIRECT
  - GEOIP,CN,DIRECT
  - MATCH,Proxies
hosts:
  n1.node.sbs: n2.node.sbs
  unrelated.io: 5.6.7.8
dns:
  fake-ip-filter: ['*.lan', n1.node.sbs, n2.node.sbs]
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, airport) }))
	defer srv.Close()

	cfg := defaultConfig()
	p := &cfg.Profiles[0]
	setSubs(&cfg, p,
		Subscription{ID: "aaaaaaaaaaaaaaaa", Name: "机场", URL: srv.URL, Enabled: true, Prefix: "[A] ", Exclude: "剩余"},
		Subscription{ID: "bbbbbbbbbbbbbbbb", Kind: "manual", Name: "自建", Enabled: true, Nodes: "- {name: 家宽, type: http, server: 9.9.9.9, port: 8080}"},
	)
	p.Sources[0].Follow = true
	p.Groups = []Group{{Name: "我的", Type: "select", Extra: []string{"Proxies"}, Nodes: "none"}}
	p.Rules = Rules{RulesBefore: "DOMAIN,mine.com,DIRECT", RuleSets: []RuleSet{{Name: "R", URL: "https://rules.example/mine.list", Policy: "我的", Enabled: true}}, RulesAfter: "MATCH,DIRECT"}

	res, err := Build(cfg, *p, NewFetcher(t.TempDir()), "", false)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Hosts  map[string]any `yaml:"hosts"`
		DNS    map[string]any `yaml:"dns"`
		Groups []struct {
			Name    string
			Proxies []string
		} `yaml:"proxy-groups"`
		Providers map[string]any `yaml:"rule-providers"`
		Rules     []string       `yaml:"rules"`
	}
	if err := yaml.Unmarshal(res.YAML, &out); err != nil {
		t.Fatal(err)
	}
	groups := map[string][]string{}
	var order []string
	for _, g := range out.Groups {
		groups[g.Name] = g.Proxies
		order = append(order, g.Name)
	}
	// 机场的组在前，本组自己的在后；和本组重名的「我的」以本组的为准
	if want := []string{"Proxies", "Auto", "香港", "过期", "我的"}; !reflect.DeepEqual(order, want) {
		t.Errorf("策略组顺序 = %v", order)
	}
	if want := []string{"Auto", "[A] 香港 01", "[A] 日本 01", "DIRECT", "家宽"}; !reflect.DeepEqual(groups["Proxies"], want) {
		t.Errorf("包含全部节点的组应换名并带上其他来源的节点: %v", groups["Proxies"])
	}
	if want := []string{"[A] 香港 01"}; !reflect.DeepEqual(groups["香港"], want) {
		t.Errorf("只含部分节点的组只换名: %v", groups["香港"])
	}
	if want := []string{"DIRECT"}; !reflect.DeepEqual(groups["过期"], want) {
		t.Errorf("节点都被过滤掉的组填 DIRECT: %v", groups["过期"])
	}
	if want := []string{"Proxies"}; !reflect.DeepEqual(groups["我的"], want) {
		t.Errorf("重名时本组自己的优先: %v", groups["我的"])
	}
	want := []string{
		"DOMAIN,mine.com,DIRECT",               // 前置规则
		"DOMAIN-SUFFIX,mine.example,我的",        // 规则集
		"DOMAIN-SUFFIX,remote.example,Proxies", // 机场规则；它自己的 DOMAIN,mine.com 和后面重复的 remote.example 被去掉
		"IP-CIDR,10.0.0.0/8,DIRECT,no-resolve",
		"GEOIP,CN,DIRECT",
		"MATCH,Proxies",
		"MATCH,DIRECT", // 后置规则（不会生效，有警告）
	}
	if !reflect.DeepEqual(out.Rules, want) || out.Providers != nil {
		t.Errorf("规则 =\n%q\n应为\n%q", out.Rules, want)
	}
	if !reflect.DeepEqual(out.Hosts, map[string]any{"n1.node.sbs": "n2.node.sbs"}) || res.Subs[0].Hosts != 1 {
		t.Errorf("hosts = %v", out.Hosts)
	}
	if ff := fmt.Sprint(out.DNS["fake-ip-filter"]); !strings.Contains(ff, "n1.node.sbs n2.node.sbs") {
		t.Errorf("fake-ip-filter = %v", ff)
	}
	joined := strings.Join(res.Warnings, "\n")
	for _, w := range []string{"「过期」里的节点都被过滤掉了", "RULE-SET,gone,DIRECT", "后置规则不会生效"} {
		if !strings.Contains(joined, w) {
			t.Errorf("缺少警告「%s」：%v", w, res.Warnings)
		}
	}
	if len(res.RuleSets) != 3 || res.RuleSets[1].Name != "机场 · remote" || res.RuleSets[2].Count != 1 {
		t.Errorf("规则集状态 = %+v", res.RuleSets)
	}
}

// 每个订阅组只能沿用一个机场：多勾的只留第一个远程来源
func TestValidateSingleFollow(t *testing.T) {
	cfg := defaultConfig()
	cfg.Sources = []Source{
		{ID: "aaaaaaaaaaaaaaaa", Name: "手动", Kind: "manual", Nodes: "- {name: x, type: http, server: 1.1.1.1, port: 80}"},
		{ID: "bbbbbbbbbbbbbbbb", Name: "A", URL: "https://a.example/s"},
		{ID: "cccccccccccccccc", Name: "B", URL: "https://b.example/s"},
	}
	p := &cfg.Profiles[0]
	p.Sources = []SourceRef{{Source: "aaaaaaaaaaaaaaaa", Follow: true}, {Source: "bbbbbbbbbbbbbbbb", Follow: true}, {Source: "cccccccccccccccc", Follow: true}}
	if err := validateConfig(&cfg, nil); err != nil {
		t.Fatal(err)
	}
	var got []bool
	for _, r := range cfg.Profiles[0].Sources {
		got = append(got, r.Follow)
	}
	if want := []bool{false, true, false}; !reflect.DeepEqual(got, want) {
		t.Errorf("follow = %v，应为 %v", got, want)
	}
}

// v7：旧数据不细致迁移，有问题的直接清掉
func TestMigrateV7(t *testing.T) {
	dir := t.TempDir()
	const meta = "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/"
	mrs := `{"name":"geosite-cn","url":"` + meta + `cn.mrs","behavior":"domain","format":"mrs","policy":"DIRECT","noResolve":false,"enabled":true}`
	keep := `{"name":"OpenAI","url":"https://r.example/OpenAI.yaml","behavior":"classical","format":"yaml","policy":"P","noResolve":true,"enabled":true}`
	old := `{"auth":{"hash":"h","salt":"s"},"config":{"version":6,"baseConfig":"mode: rule","sources":[],
		"templates":[{"id":"3333333333333333","name":"含 mrs","kind":"rules","ruleSets":[` + keep + `,` + mrs + `]},
		 {"id":"6666666666666666","name":"精简规则（Loyalsoldier）","kind":"rules","ruleSets":[{"name":"ls-reject","url":"https://raw.githubusercontent.com/Loyalsoldier/clash-rules/release/reject.txt","policy":"R","enabled":true}]},
		 {"id":"7777777777777777","name":"正常","kind":"rules","ruleSets":[` + keep + `]}],
		"ruleSources":[{"id":"4444444444444444","name":"自己加的","url":"` + meta + `{name}.mrs","ext":".mrs","behavior":"domain","format":"mrs","ipcidr":"","pinned":["cn"]}],
		"profiles":[{"id":"1111111111111111","name":"P","token":"t1","ruleSets":[` + keep + `,` + mrs + `],"applied":{"rules":{"id":"7777777777777777","hash":"x"},"groups":{"id":"8888888888888888","hash":"y"}}}]}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := s.Config()
	if len(c.RuleSources) != len(seedRuleSources()) || c.RuleSources[0].Name != seedRuleSources()[0].Name {
		t.Errorf("规则仓库应换成新的内置列表: %+v", c.RuleSources)
	}
	if len(c.Templates) != 1 || c.Templates[0].Name != "正常" {
		t.Errorf("含 mrs / Loyalsoldier 的规则模板应当去掉: %+v", c.Templates)
	}
	p := c.Profiles[0]
	if want := []RuleSet{{Name: "OpenAI", URL: "https://r.example/OpenAI.yaml", Policy: "P", Enabled: true}}; !reflect.DeepEqual(p.RuleSets, want) {
		t.Errorf("订阅组的规则集 = %+v", p.RuleSets)
	}
	if _, ok := p.Applied["rules"]; ok || p.Applied["groups"].ID != "8888888888888888" {
		t.Errorf("只清规则模板的套用记录: %+v", p.Applied)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if slices.ContainsFunc([]string{`"behavior"`, `"noResolve"`, `"ipcidr"`, `"format"`}, func(k string) bool { return strings.Contains(string(b), k) }) {
		t.Errorf("旧字段没有去掉")
	}
}
