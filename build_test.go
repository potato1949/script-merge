package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"
)

// 底板机场：完整的 Clash 配置，带策略组、规则、rule-providers、proxy-providers、DNS 和 hosts
const baseAirport = `mixed-port: 7890
dns:
  enable: true
  fake-ip-filter: ['*.lan']
hosts:
  own.example: 1.2.3.4
proxy-providers:
  pp: {type: http, url: 'https://pp.example/p.yaml', path: ./pp.yaml}
proxies:
  - {name: 香港 01, type: ss, server: a.com, port: 443, cipher: aes-128-gcm, password: 'true'}
  - {name: 日本 01, type: ss, server: b.com, port: 443, cipher: aes-128-gcm, password: x}
  - {name: 剩余流量：1G, type: ss, server: c.com, port: 1, cipher: aes-128-gcm, password: x}
proxy-groups:
  - {name: 🚀 节点选择, type: select, proxies: [♻️ 自动选择, 香港 01, 日本 01, 剩余流量：1G, DIRECT]}
  - {name: ♻️ 自动选择, type: url-test, url: 'http://t', interval: 300, proxies: [香港 01, 日本 01, 剩余流量：1G]}
  - {name: 🇭🇰 香港, type: select, proxies: [香港 01]}
  - {name: 过期, type: select, proxies: [剩余流量：1G]}
  - {name: 提供者, type: select, use: [pp]}
rule-providers:
  r: {type: http, behavior: domain, url: 'https://rules.example/r.yaml', path: ./r.yaml, interval: 86400}
rules:
  - RULE-SET,r,🚀 节点选择
  - DOMAIN,a.com,🇭🇰 香港
  - GEOIP,CN,DIRECT
  - MATCH,🚀 节点选择
`

// 其他机场：节点地址经 hosts 映射；它自己的策略组和规则不会被用到
const otherAirport = `proxies:
  - {name: 香港 01, type: trojan, server: n1.node.sbs, port: 443, password: x}
proxy-groups:
  - {name: 不会出现, type: select, proxies: [香港 01]}
rules:
  - MATCH,不会出现
hosts:
  n1.node.sbs: n2.node.sbs
  n2.node.sbs: 5.6.7.8
  unrelated.io: 9.9.9.9
dns:
  fake-ip-filter: [n1.node.sbs, n2.node.sbs, '*.lan']
`

type outConfig struct {
	MixedPort int              `yaml:"mixed-port"`
	DNS       map[string]any   `yaml:"dns"`
	Hosts     map[string]any   `yaml:"hosts"`
	Proxies   []map[string]any `yaml:"proxies"`
	Groups    []struct {
		Name    string
		Proxies []string
		Use     []string
	} `yaml:"proxy-groups"`
	ProxyProviders map[string]any `yaml:"proxy-providers"`
	RuleProviders  map[string]any `yaml:"rule-providers"`
	Rules          []string       `yaml:"rules"`
}

func parseOut(t *testing.T, res *BuildResult) outConfig {
	t.Helper()
	var out outConfig
	if err := yaml.Unmarshal(res.YAML, &out); err != nil {
		t.Fatalf("输出不是合法 YAML: %v\n%s", err, res.YAML)
	}
	return out
}

func TestBuildEndToEnd(t *testing.T) {
	var gotUA string
	var mu sync.Mutex
	var unlockUAs []string // 解锁地址收到的 UA，按请求顺序
	var unlocked atomic.Bool
	var otherHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/unlock":
			mu.Lock()
			unlockUAs = append(unlockUAs, r.UserAgent())
			mu.Unlock()
			unlocked.Store(true)
		case "/base":
			if !unlocked.Load() { // 解锁地址被访问过之后才放行
				http.Error(w, "locked", http.StatusForbidden)
				return
			}
			gotUA = r.UserAgent()
			w.Header().Set("Subscription-Userinfo", "upload=1; download=2; total=100; expire=2000")
			fmt.Fprint(w, baseAirport)
		case "/other":
			otherHits.Add(1)
			w.Header().Set("Subscription-Userinfo", "upload=10; download=20; total=1000; expire=1000")
			fmt.Fprint(w, otherAirport)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	cfg := testConfig()
	cfg.UserAgent = "clash.meta"
	p := &cfg.Profiles[0]
	addSource(&cfg, p, Source{ID: "bbbbbbbbbbbbbbbb", Name: "其他", URL: upstream.URL + "/other"}, SourceRef{Enabled: true})
	// 底板排在第二个、而且没勾启用：仍然使用，并且它的节点排在最前面
	addSource(&cfg, p, Source{ID: "aaaaaaaaaaaaaaaa", Name: "底板", URL: upstream.URL + "/base", UnlockURL: upstream.URL + "/unlock"}, SourceRef{Exclude: "剩余"})
	addSource(&cfg, p, Source{ID: "cccccccccccccccc", Name: "挂了", URL: upstream.URL + "/missing"}, SourceRef{Enabled: true})
	// 同一个来源再引用一次（加前缀）：不会再请求一次上游，流量信息也不重复计算
	p.Sources = append(p.Sources, SourceRef{Source: "bbbbbbbbbbbbbbbb", Enabled: true, Prefix: "[B] "})
	addSource(&cfg, p, Source{ID: "eeeeeeeeeeeeeeee", Kind: "manual", Name: "手动",
		Nodes: "- {name: 自建, type: http, server: 9.9.9.9, port: 8080}\n- {name: 排除我, type: http, server: 9.9.9.8, port: 8080}"},
		SourceRef{Enabled: true, Prefix: "[M] ", Exclude: "排除"})
	addSource(&cfg, p, Source{ID: "ffffffffffffffff", Name: "停用", URL: upstream.URL + "/other"}, SourceRef{})
	p.Base = "aaaaaaaaaaaaaaaa"
	p.Parser = "yaml:\n  prepend-rules:\n    - DOMAIN,my.com,DIRECT\n  commands:\n    - dns.enable=false\n"

	f := NewFetcher(t.TempDir())
	res, err := Build(cfg, *p, f, FetchAuto)
	if err != nil {
		t.Fatal(err)
	}
	if gotUA != "clash.meta" {
		t.Errorf("UA = %q", gotUA)
	}
	seenUnlockUAs := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), unlockUAs...)
	}
	if got := seenUnlockUAs(); !reflect.DeepEqual(got, []string{chromeUA}) { // 没设置解锁 UA 时用内置的 Chrome UA
		t.Errorf("解锁请求 UA = %q", got)
	}
	// 底板排第一个；停用的引用不出现；Index 是引用在订阅组里的下标
	if len(res.Subs) != 5 || !res.Subs[0].Base || res.Subs[0].Index != 1 || res.Subs[0].Used != 2 ||
		res.Subs[1].ID != "bbbbbbbbbbbbbbbb" || res.Subs[1].Hosts != 2 || res.Subs[2].Error == "" || res.Subs[3].Index != 3 {
		t.Errorf("来源状态不符: %+v", res.Subs)
	}
	if n := otherHits.Load(); n != 1 {
		t.Errorf("同一个来源被引用两次，上游应只被请求 1 次，实际 %d 次", n)
	}
	if res.Userinfo != "upload=11; download=22; total=1100; expire=1000" {
		t.Errorf("userinfo = %q", res.Userinfo)
	}

	out := parseOut(t, res)
	if strings.Contains(string(res.YAML), `\U`) {
		t.Errorf("emoji 被转义输出")
	}
	var names []string
	for _, p := range out.Proxies {
		names = append(names, p["name"].(string))
	}
	if want := []string{"香港 01", "日本 01", "香港 01 2", "[B] 香港 01", "[M] 自建"}; !reflect.DeepEqual(names, want) {
		t.Errorf("节点 = %v", names)
	}
	if out.Proxies[0]["password"] != "true" { // 字符串 "true" 不能被输出成布尔值
		t.Errorf("password 类型丢失: %#v", out.Proxies[0]["password"])
	}
	groups := map[string][]string{}
	var order []string
	for _, g := range out.Groups {
		groups[g.Name] = g.Proxies
		order = append(order, g.Name)
	}
	if want := []string{"🚀 节点选择", "♻️ 自动选择", "🇭🇰 香港", "过期", "提供者"}; !reflect.DeepEqual(order, want) {
		t.Errorf("策略组应只有底板的: %v", order)
	}
	others := []string{"香港 01 2", "[B] 香港 01", "[M] 自建"}
	if want := append([]string{"♻️ 自动选择", "香港 01", "日本 01", "DIRECT"}, others...); !reflect.DeepEqual(groups["🚀 节点选择"], want) {
		t.Errorf("包含底板全部节点的组应带上其他来源的节点: %v", groups["🚀 节点选择"])
	}
	if want := append([]string{"香港 01", "日本 01"}, others...); !reflect.DeepEqual(groups["♻️ 自动选择"], want) {
		t.Errorf("自动选择 = %v", groups["♻️ 自动选择"])
	}
	if !reflect.DeepEqual(groups["🇭🇰 香港"], []string{"香港 01"}) || !reflect.DeepEqual(groups["过期"], []string{"DIRECT"}) {
		t.Errorf("部分节点的组只换名，节点全被过滤的填 DIRECT: %v / %v", groups["🇭🇰 香港"], groups["过期"])
	}
	if g := out.Groups[4]; g.Proxies != nil || !reflect.DeepEqual(g.Use, []string{"pp"}) || out.ProxyProviders["pp"] == nil {
		t.Errorf("用 proxy-providers 的组应原样保留: %+v", g)
	}
	// 底板的规则、rule-providers 和其他设置原样保留；Parser 在最后执行
	if want := []string{"DOMAIN,my.com,DIRECT", "RULE-SET,r,🚀 节点选择", "DOMAIN,a.com,🇭🇰 香港", "GEOIP,CN,DIRECT", "MATCH,🚀 节点选择"}; !reflect.DeepEqual(out.Rules, want) {
		t.Errorf("规则 = %q", out.Rules)
	}
	if out.RuleProviders["r"] == nil || out.MixedPort != 7890 || out.DNS["enable"] != false {
		t.Errorf("底板的其余内容应保留: %+v", out)
	}
	// 底板自己的 hosts 全部保留；其他来源只带上和它的节点有关的
	if want := map[string]any{"own.example": "1.2.3.4", "n1.node.sbs": "n2.node.sbs", "n2.node.sbs": "5.6.7.8"}; !reflect.DeepEqual(out.Hosts, want) {
		t.Errorf("hosts = %v", out.Hosts)
	}
	if ff := fmt.Sprint(out.DNS["fake-ip-filter"]); ff != "[*.lan n1.node.sbs n2.node.sbs]" {
		t.Errorf("fake-ip-filter = %v", ff)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "「过期」") {
		t.Errorf("警告 = %v", res.Warnings)
	}
	if res.Total != 5 || res.Groups != 5 || res.Rules != 5 {
		t.Errorf("统计 = %d / %d / %d", res.Total, res.Groups, res.Rules)
	}

	// 改了全局的解锁 UA：缓存键随之改变，会重新解锁并拉取
	cfg.UnlockUserAgent = "Unlock/1.0"
	if _, err := Build(cfg, *p, f, FetchAuto); err != nil {
		t.Fatal(err)
	}
	if got := seenUnlockUAs(); !reflect.DeepEqual(got, []string{chromeUA, "Unlock/1.0"}) {
		t.Errorf("自定义解锁 UA 未生效: %q", got)
	}

	// 上游挂掉后应回退到缓存（CacheMinutes=0 强制重新拉取）；缓存在磁盘上存过一轮，内容不变
	upstream.Close()
	cfg.CacheMinutes = 0
	res2, err := Build(cfg, *p, NewFetcher(filepath.Dir(f.dir)), FetchAuto)
	if err != nil || res2.Total != 5 || !res2.Subs[0].Stale {
		t.Fatalf("缓存回退失败: err=%v subs=%+v", err, res2)
	}
	if out2 := parseOut(t, res2); !reflect.DeepEqual(out2, out) {
		t.Errorf("从磁盘缓存生成的结果不同:\n%s\n---\n%s", res2.YAML, res.YAML)
	}
}

// 底板不可用时直接报错，不输出残缺的配置。
func TestBuildBaseErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/clash":
			fmt.Fprint(w, baseAirport)
		case "/links": // 只有节点、没有策略组
			fmt.Fprint(w, "ss://YWVzLTI1Ni1nY206cGFzcw@1.2.3.4:8388#n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	build := func(base string, edit func(*Config, *Profile)) error {
		t.Helper()
		cfg := testConfig()
		p := &cfg.Profiles[0]
		addSource(&cfg, p, Source{ID: "aaaaaaaaaaaaaaaa", Name: "A", URL: srv.URL + base}, SourceRef{Enabled: true})
		addSource(&cfg, p, Source{ID: "bbbbbbbbbbbbbbbb", Kind: "manual", Name: "M", Nodes: "- {name: x, type: http, server: 1.1.1.1, port: 80}"}, SourceRef{Enabled: true})
		p.Base = "aaaaaaaaaaaaaaaa"
		if edit != nil {
			edit(&cfg, p)
		}
		_, err := Build(cfg, *p, NewFetcher(t.TempDir()), FetchAuto)
		return err
	}
	if err := build("/clash", nil); err != nil {
		t.Fatalf("正常情况: %v", err)
	}
	cases := map[string]struct {
		path string
		edit func(*Config, *Profile)
		want string
	}{
		"没选底板":    {"/clash", func(c *Config, p *Profile) { p.Base = "" }, "还没有选择底板"},
		"底板是手动节点": {"/clash", func(c *Config, p *Profile) { p.Base = "bbbbbbbbbbbbbbbb" }, "还没有选择底板"},
		"底板不在本组":  {"/clash", func(c *Config, p *Profile) { p.Sources = p.Sources[1:] }, "还没有选择底板"},
		"没有策略组":   {"/links", nil, "不是带策略组的 Clash 配置"},
		"拉取失败":    {"/gone", nil, "拉取失败"},
	}
	for name, c := range cases {
		if err := build(c.path, c.edit); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestFetchUsesProxy(t *testing.T) {
	proxyHits.Lock()
	proxyHits.urls = nil // -count=N 时每轮重新计
	proxyHits.Unlock()
	cfg := testConfig()
	p := &cfg.Profiles[0]
	addSource(&cfg, p, Source{ID: "aaaaaaaaaaaaaaaa", Name: "经代理", URL: "http://upstream.test/sub", UnlockURL: "http://upstream.test/unlock"}, SourceRef{Enabled: true})
	p.Base = "aaaaaaaaaaaaaaaa"
	res, err := Build(cfg, *p, NewFetcher(t.TempDir()), FetchAuto)
	if err != nil || res.Total != 1 || res.Subs[0].Error != "" {
		t.Fatalf("经代理拉取失败: err=%v res=%+v", err, res)
	}
	// 解锁请求和订阅请求都应到达 HTTP_PROXY 指向的代理（upstream.test 这个域名并不存在，直连必然失败）
	proxyHits.Lock()
	defer proxyHits.Unlock()
	if want := []string{"http://upstream.test/unlock " + chromeUA, "http://upstream.test/sub " + defaultUA}; !reflect.DeepEqual(proxyHits.urls, want) {
		t.Errorf("代理收到的请求 = %q", proxyHits.urls)
	}
}

// 前置代理（代理链）：引用上填了 Dialer，这批节点都带上 dialer-proxy；目标不存在、或能绕回节点自己时给出警告。
func TestDialerProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `proxies:
  - {name: 香港 01, type: ss, server: a.com, port: 443, cipher: aes-128-gcm, password: x}
  - {name: 日本 01, type: ss, server: b.com, port: 443, cipher: aes-128-gcm, password: x}
proxy-groups:
  - {name: 跳板, type: select, proxies: [香港 01]}
  - {name: 全部, type: select, proxies: [跳板, 香港 01, 日本 01]}
rules:
  - MATCH,全部
`)
	}))
	defer srv.Close()
	cfg := testConfig()
	p := &cfg.Profiles[0]
	addSource(&cfg, p, Source{ID: "aaaaaaaaaaaaaaaa", Name: "机场", URL: srv.URL}, SourceRef{Enabled: true})
	addSource(&cfg, p, Source{ID: "bbbbbbbbbbbbbbbb", Kind: "manual", Name: "家宽",
		Nodes: "- {name: 落地, type: http, server: 9.9.9.9, port: 8080, dialer-proxy: 旧值}"}, SourceRef{Enabled: true, Prefix: "家宽 "})
	p.Base = "aaaaaaaaaaaaaaaa"
	f := NewFetcher(t.TempDir())
	build := func(dialer string) (map[string]any, []string) {
		t.Helper()
		p.Sources[1].Dialer = dialer
		res, err := Build(cfg, *p, f, FetchAuto)
		if err != nil {
			t.Fatal(err)
		}
		nodes := map[string]any{}
		for _, n := range parseOut(t, res).Proxies {
			nodes[fmt.Sprint(n["name"])] = n["dialer-proxy"]
		}
		return nodes, res.Warnings
	}

	nodes, warns := build(" 跳板 ")
	if nodes["家宽 落地"] != "跳板" || nodes["香港 01"] != nil {
		t.Errorf("dialer-proxy 不符: %v", nodes)
	}
	if len(warns) != 0 {
		t.Errorf("不该有警告: %v", warns)
	}

	// 不填：保留节点自带的值；目标不存在要警告
	nodes, warns = build("")
	if nodes["家宽 落地"] != "旧值" || len(warns) != 1 || !strings.Contains(warns[0], "「旧值」不存在") {
		t.Errorf("自带的 dialer-proxy: %v, 警告: %v", nodes, warns)
	}

	// 「全部」里（直接或经由其他策略组）能选到落地节点自己 → 环
	_, warns = build("全部")
	if len(warns) != 1 || !strings.Contains(warns[0], "「家宽 落地」") || !strings.Contains(warns[0], "环") {
		t.Errorf("应警告形成环: %v", warns)
	}
	// 直接填节点名可以；填自己是环
	if _, warns = build("香港 01"); len(warns) != 0 {
		t.Errorf("填节点名不该有警告: %v", warns)
	}
	if _, warns = build("家宽 落地"); len(warns) != 1 {
		t.Errorf("填自己应警告: %v", warns)
	}
}
