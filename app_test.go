package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestMain 在任何请求发出之前设置代理环境变量：http.ProxyFromEnvironment 只在第一次使用时读一次环境。
// 其他测试的上游都在 127.0.0.1 上，标准库对回环地址从不走代理，所以不受影响。
var proxyHits struct {
	sync.Mutex
	urls []string
}

func TestMain(m *testing.M) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Lock()
		proxyHits.urls = append(proxyHits.urls, r.URL.String()+" "+r.UserAgent())
		proxyHits.Unlock()
		if r.URL.Host != "upstream.test" { // 经代理的请求带完整地址
			http.Error(w, "unexpected host", http.StatusBadGateway)
			return
		}
		if r.URL.Path == "/sub" {
			fmt.Fprint(w, "proxies:\n  - {name: via-proxy, type: ss, server: a.com, port: 443}\n")
		}
	}))
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"} {
		os.Unsetenv(k)
	}
	os.Setenv("HTTP_PROXY", proxy.URL)
	code := m.Run()
	proxy.Close()
	os.Exit(code)
}

// setSubs 用 v4 的「订阅」写法给订阅组配置节点来源：每条订阅变成一个全局来源加一条引用。
func setSubs(cfg *Config, p *Profile, subs ...Subscription) {
	for _, s := range subs {
		src, ref := splitSubscription(s)
		cfg.Sources = append(cfg.Sources, src)
		p.Sources = append(p.Sources, ref)
	}
}

func testRoot() map[string]any {
	return map[string]any{
		"dns": map[string]any{"enable": true},
		"proxies": []any{
			map[string]any{"name": "HK 01", "type": "ss"},
			map[string]any{"name": "US 01", "type": "ss"},
			map[string]any{"name": "debug", "type": "http", "udp": false},
		},
		"proxy-groups": []any{
			map[string]any{"name": "G1", "type": "select", "proxies": []any{"HK 01", "US 01", "debug"}},
			map[string]any{"name": "a.b+c", "type": "select", "proxies": []any{"DIRECT"}},
		},
		"rules": []any{"DOMAIN,a.com,DIRECT", "MATCH,G1"},
	}
}

func groupProxies(root map[string]any, i int) []any {
	return root["proxy-groups"].([]any)[i].(map[string]any)["proxies"].([]any)
}

func TestCommands(t *testing.T) {
	cases := []struct {
		cmd   string
		check func(map[string]any) bool
	}{
		{"dns.enable=false", func(r map[string]any) bool { return r["dns"].(map[string]any)["enable"] == false }},
		{"dns.port=53", func(r map[string]any) bool { return r["dns"].(map[string]any)["port"] == 53 }},
		{"proxy-groups.0.proxies.2+DIRECT", func(r map[string]any) bool {
			return reflect.DeepEqual(groupProxies(r, 0), []any{"HK 01", "US 01", "DIRECT", "debug"})
		}},
		{"proxy-groups.0.proxies.0=DIRECT", func(r map[string]any) bool { return groupProxies(r, 0)[0] == "DIRECT" }},
		{"proxies.0-", func(r map[string]any) bool { return len(r["proxies"].([]any)) == 2 }},
		{"proxies.(debug).udp=true", func(r map[string]any) bool {
			return r["proxies"].([]any)[2].(map[string]any)["udp"] == true
		}},
		{"proxy-groups.G1.proxies=[]proxyNames|HK", func(r map[string]any) bool {
			return reflect.DeepEqual(groupProxies(r, 0), []any{"HK 01"})
		}},
		{"proxy-groups.(a.b+c).proxies+[]proxyNames|^(?=.*(?:US|debug)).*$", func(r map[string]any) bool {
			return reflect.DeepEqual(groupProxies(r, 1), []any{"DIRECT", "US 01", "debug"})
		}},
		{"proxy-groups.G1.proxies.0+[]groupNames|^a", func(r map[string]any) bool {
			return groupProxies(r, 0)[0] == "a.b+c" && len(groupProxies(r, 0)) == 4
		}},
		{"proxy-groups.G1.proxies_US 01", func(r map[string]any) bool {
			return reflect.DeepEqual(groupProxies(r, 0), []any{"HK 01", "debug"})
		}},
		{"proxy-groups.G1.proxies_[]proxyNames|01", func(r map[string]any) bool {
			return reflect.DeepEqual(groupProxies(r, 0), []any{"debug"})
		}},
		{"rules+DOMAIN,b.com,G1", func(r map[string]any) bool { return len(r["rules"].([]any)) == 3 }},
		{"rules_DOMAIN,a.com,DIRECT", func(r map[string]any) bool {
			return reflect.DeepEqual(r["rules"], []any{"MATCH,G1"})
		}},
		{"proxy-groups.(G1)-", func(r map[string]any) bool { return len(r["proxy-groups"].([]any)) == 1 }},
		{`tun={"enable":true,"mtu":1500}`, func(r map[string]any) bool {
			return reflect.DeepEqual(r["tun"], map[string]any{"enable": true, "mtu": 1500})
		}},
	}
	for _, c := range cases {
		root := testRoot()
		if err := runCommand(root, c.cmd); err != nil {
			t.Errorf("%s: %v", c.cmd, err)
		} else if !c.check(root) {
			t.Errorf("%s: 结果不符: %v", c.cmd, root)
		}
	}
	for _, bad := range []string{"proxy-groups.nope.proxies+X", "noop", "proxies.9-"} {
		if err := runCommand(testRoot(), bad); err == nil {
			t.Errorf("%s: 应当报错", bad)
		}
	}
}

func TestParserOrder(t *testing.T) {
	root := testRoot()
	warns, err := ApplyParser(root, `
parsers:
  - url: https://example.com
    yaml:
      commands:
        - proxy-groups.new.proxies=[]proxyNames|test
      prepend-rules:
        - DOMAIN,test.com,DIRECT
      append-proxies:
        - {name: test, type: http, server: 1.1.1.1, port: 80}
      prepend-proxy-groups:
        - {name: new, type: fallback, proxies: [DIRECT]}
      mix-rule-providers:
        x: {type: http}
      mix-object:
        mode: global
`)
	if err != nil || len(warns) > 0 {
		t.Fatal(err, warns)
	}
	if root["rules"].([]any)[0] != "DOMAIN,test.com,DIRECT" || root["mode"] != "global" ||
		!reflect.DeepEqual(groupProxies(root, 0), []any{"test"}) ||
		root["rule-providers"].(map[string]any)["x"] == nil {
		t.Errorf("结果不符: %v", root)
	}
}

func TestURIs(t *testing.T) {
	vmess := "vmess://" + base64.StdEncoding.EncodeToString([]byte(
		`{"v":"2","ps":"vm","add":"v.example.com","port":"443","id":"uuid-1","aid":0,"net":"ws","host":"h.example.com","path":"/ws","tls":"tls"}`))
	ss := "ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:pass")) + "@1.2.3.4:8388#%E9%A6%99%E6%B8%AF"
	lines := []string{
		ss, vmess,
		"trojan://pw@t.example.com:443?sni=s.example.com&allowInsecure=1#tj",
		"vless://uuid-2@vl.example.com:443?security=reality&pbk=KEY&sid=ab&sni=x.com&fp=chrome&flow=xtls-rprx-vision&type=tcp#vl",
		"hysteria2://pw@hy.example.com:8443?sni=x.com&insecure=1#hy",
		"unknown://x",
	}
	body := base64.StdEncoding.EncodeToString([]byte(strings.Join(lines, "\n")))
	ps, err := parseSubscription([]byte(body))
	if err != nil || len(ps) != 5 {
		t.Fatalf("got %d, err %v", len(ps), err)
	}
	if ps[0]["name"] != "香港" || ps[0]["cipher"] != "aes-256-gcm" || ps[0]["port"] != 8388 {
		t.Errorf("ss: %v", ps[0])
	}
	if ps[1]["network"] != "ws" || ps[1]["tls"] != true || ps[1]["servername"] != "h.example.com" {
		t.Errorf("vmess: %v", ps[1])
	}
	if ps[2]["skip-cert-verify"] != true || ps[2]["sni"] != "s.example.com" {
		t.Errorf("trojan: %v", ps[2])
	}
	if ps[3]["reality-opts"].(map[string]any)["public-key"] != "KEY" || ps[3]["flow"] != "xtls-rprx-vision" {
		t.Errorf("vless: %v", ps[3])
	}
	if ps[4]["type"] != "hysteria2" || ps[4]["port"] != 8443 {
		t.Errorf("hy2: %v", ps[4])
	}
}

func TestBuildEndToEnd(t *testing.T) {
	var gotUA string
	var mu sync.Mutex
	var unlockUAs []string // 解锁地址收到的 UA，按请求顺序
	var unlocked atomic.Bool
	var plainHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/unlock":
			mu.Lock()
			unlockUAs = append(unlockUAs, r.UserAgent())
			mu.Unlock()
			unlocked.Store(true)
		case "/locked":
			if !unlocked.Load() { // 解锁地址被访问过之后才放行
				http.Error(w, "locked", http.StatusForbidden)
				return
			}
			gotUA = r.UserAgent()
			w.Header().Set("Subscription-Userinfo", "upload=1; download=2; total=100; expire=2000")
			fmt.Fprint(w, "proxies:\n  - {name: 香港 01, type: ss, server: a.com, port: 443, cipher: aes-128-gcm, password: 'true'}\n  - {name: 过期提醒, type: ss, server: b.com, port: 1}\n")
		case "/plain":
			plainHits.Add(1)
			w.Header().Set("Subscription-Userinfo", "upload=10; download=20; total=1000; expire=1000")
			fmt.Fprint(w, "proxies:\n  - {name: 香港 01, type: trojan, server: c.com, port: 443, password: x}\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	cfg := defaultConfig()
	cfg.UserAgent = "ClashforWindows/0.20.39" // 全局固定 UA
	p := &cfg.Profiles[0]
	setSubs(&cfg, p,
		Subscription{ID: "aaaaaaaaaaaaaaaa", Name: "A", URL: upstream.URL + "/locked", UnlockURL: upstream.URL + "/unlock", Enabled: true, Exclude: "过期"},
		Subscription{ID: "bbbbbbbbbbbbbbbb", Name: "B", URL: upstream.URL + "/plain", Enabled: true, Prefix: "[B] "},
		Subscription{ID: "cccccccccccccccc", Name: "C", URL: upstream.URL + "/missing", Enabled: true}, // 上游 404 → 失败
		Subscription{ID: "eeeeeeeeeeeeeeee", Kind: "manual", Name: "手动", Enabled: true, Prefix: "[M] ",
			Nodes: "- {name: 自建, type: http, server: 9.9.9.9, port: 8080}\n- {name: 排除我, type: http, server: 9.9.9.8, port: 8080}", Exclude: "排除"},
		Subscription{ID: "ffffffffffffffff", Name: "停用", URL: upstream.URL + "/plain"},
	)
	// 同一个来源在本组里再引用一次（不加前缀）：节点与 A 重名，且不会再请求一次上游
	p.Sources = append(p.Sources[:3:3], append([]SourceRef{{Source: "bbbbbbbbbbbbbbbb", Enabled: true}}, p.Sources[3:]...)...)
	p.Groups = append(p.Groups, Group{Name: "🇭🇰 香港", Type: "url-test", Nodes: "filter", Filter: "^(?!.*B).*香港"})
	p.RulesBefore = "# 注释\n- DOMAIN,my.com,🇭🇰 香港\nDOMAIN,bad.com,不存在"
	p.Parser = "yaml:\n  commands:\n    - dns.enable=false\n    - proxy-groups.(🇭🇰 香港).proxies+[]proxyNames|自建\n" // Parser 能引用手动节点

	f := NewFetcher(t.TempDir())
	res, err := Build(cfg, *p, f, "mihomo/1.0", false)
	if err != nil {
		t.Fatal(err)
	}
	if gotUA != "ClashforWindows/0.20.39" {
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

	// 改了全局的解锁 UA：缓存键随之改变，会重新解锁并拉取
	cfg.UnlockUserAgent = "Unlock/1.0"
	if _, err := Build(cfg, *p, f, "mihomo/1.0", false); err != nil {
		t.Fatal(err)
	}
	if got := seenUnlockUAs(); !reflect.DeepEqual(got, []string{chromeUA, "Unlock/1.0"}) {
		t.Errorf("自定义解锁 UA 未生效: %q", got)
	}
	if res.Subs[0].Used != 1 || res.Subs[0].Error != "" || res.Subs[2].Error == "" {
		t.Errorf("订阅状态不符: %+v", res.Subs)
	}
	// 停用的引用不出现在状态里；Index 是引用在订阅组里的下标，ID 是来源 ID
	if len(res.Subs) != 5 || res.Subs[3].Index != 3 || res.Subs[3].ID != "bbbbbbbbbbbbbbbb" || res.Subs[4].Name != "手动" {
		t.Errorf("来源状态的下标不符: %+v", res.Subs)
	}
	if n := plainHits.Load(); n != 1 {
		t.Errorf("同一个来源被引用两次，上游应只被请求 1 次，实际 %d 次", n)
	}
	if res.Userinfo != "upload=21; download=42; total=2100; expire=1000" {
		t.Errorf("userinfo = %q", res.Userinfo)
	}

	var out struct {
		DNS     map[string]any   `yaml:"dns"`
		Proxies []map[string]any `yaml:"proxies"`
		Groups  []struct {
			Name    string
			Proxies []string
		} `yaml:"proxy-groups"`
		Providers map[string]any `yaml:"rule-providers"`
		Rules     []string       `yaml:"rules"`
	}
	if err := yaml.Unmarshal(res.YAML, &out); err != nil {
		t.Fatalf("输出不是合法 YAML: %v\n%s", err, res.YAML)
	}
	if strings.Contains(string(res.YAML), `\U`) || !strings.Contains(string(res.YAML), gProxy) {
		t.Errorf("emoji 被转义输出")
	}
	var names []string
	for _, p := range out.Proxies {
		names = append(names, p["name"].(string))
	}
	if want := []string{"香港 01", "[B] 香港 01", "香港 01 2", "[M] 自建"}; !reflect.DeepEqual(names, want) {
		t.Errorf("节点 = %v", names)
	}
	if out.Proxies[0]["password"] != "true" { // 字符串 "true" 不能被输出成布尔值
		t.Errorf("password 类型丢失: %#v", out.Proxies[0]["password"])
	}
	last := out.Groups[len(out.Groups)-1]
	if !reflect.DeepEqual(last.Proxies, []string{"香港 01", "香港 01 2", "[M] 自建"}) {
		t.Errorf("香港组 = %v", last.Proxies)
	}
	if out.DNS["enable"] != false || out.Providers["OpenAI"] == nil {
		t.Errorf("parser 或规则集未生效")
	}
	if out.Rules[0] != "DOMAIN,my.com,🇭🇰 香港" || out.Rules[len(out.Rules)-1] != "MATCH,"+gFinal {
		t.Errorf("规则 = %v", out.Rules)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "不存在") {
		t.Errorf("警告 = %v", res.Warnings)
	}

	// 上游挂掉后应回退到缓存（CacheMinutes=0 强制重新拉取）
	upstream.Close()
	cfg.CacheMinutes = 0
	res2, err := Build(cfg, *p, f, "mihomo/1.0", false)
	if err != nil || res2.Total != 4 || !res2.Subs[0].Stale {
		t.Errorf("缓存回退失败: total=%d err=%v subs=%+v", res2.Total, err, res2.Subs)
	}
}

func TestUAPassthrough(t *testing.T) {
	hits := map[string]int{} // "路径 UA" → 次数
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path+" "+r.UserAgent()]++
		mu.Unlock()
		fmt.Fprintf(w, "proxies:\n  - {name: %s, type: ss, server: a.com, port: 443}\n", strings.TrimPrefix(r.URL.Path, "/"))
	}))
	defer upstream.Close()

	cfg := defaultConfig()
	cfg.CacheMinutes = 5
	cfg.UserAgent = "fixed/1" // 全局固定 UA：固定模式用它，透传模式下客户端没带 UA 时也用它
	p := &cfg.Profiles[0]
	setSubs(&cfg, p,
		Subscription{ID: "aaaaaaaaaaaaaaaa", Name: "透传", URL: upstream.URL + "/default", Enabled: true, UAMode: "passthrough"},
		Subscription{ID: "bbbbbbbbbbbbbbbb", Name: "固定", URL: upstream.URL + "/fixed", Enabled: true},
		Subscription{ID: "cccccccccccccccc", Name: "没填链接", Enabled: true},
	)
	f := NewFetcher(t.TempDir())
	nodes := func(ua string) string {
		res, err := Build(cfg, *p, f, ua, false)
		if err != nil {
			t.Fatal(err)
		}
		var out struct{ Proxies []struct{ Name string } }
		_ = yaml.Unmarshal(res.YAML, &out)
		var names []string
		for _, n := range out.Proxies {
			names = append(names, n.Name)
		}
		return strings.Join(names, ",")
	}
	for _, ua := range []string{"mihomo/1.19", "Shadowrocket/2615", ""} {
		if got := nodes(ua); got != "default,fixed" { // 第三个订阅没填链接，不提供节点
			t.Errorf("UA %q: %s", ua, got)
		}
	}
	nodes("mihomo/1.19") // 5 分钟内同一 UA 再拉取：全部命中缓存
	nodes("Shadowrocket/2615")
	want := map[string]int{
		"/default mihomo/1.19": 1, "/default Shadowrocket/2615": 1, "/default fixed/1": 1, // 透传：每种 UA 各拉一次，没带 UA 时用全局固定 UA
		"/fixed fixed/1": 1, // 固定 UA 的订阅不随客户端变化，只拉一次
	}
	if !reflect.DeepEqual(hits, want) {
		t.Errorf("上游请求 = %v", hits)
	}

	// 上游挂掉：同一 UA 用自己的旧缓存；没拉过的 UA 就是失败，不借用其他 UA 的结果
	upstream.Close()
	cfg.CacheMinutes = 0
	res, _ := Build(cfg, *p, f, "mihomo/1.19", false)
	if st := res.Subs[0]; !st.Stale || st.Used != 1 {
		t.Errorf("同一 UA 应使用旧缓存: %+v", st)
	}
	res, _ = Build(cfg, *p, f, "never-seen/1.0", false)
	if st := res.Subs[0]; st.Stale || st.Error == "" || st.Used != 0 {
		t.Errorf("不应回退到其他 UA 的缓存: %+v", st)
	}
	if st := res.Subs[2]; !strings.Contains(st.Error, "订阅链接") {
		t.Errorf("没填链接的订阅应报错: %+v", st)
	}
}

func TestFetchUsesProxy(t *testing.T) {
	proxyHits.Lock()
	proxyHits.urls = nil // -count=N 时每轮重新计
	proxyHits.Unlock()
	cfg := defaultConfig()
	p := &cfg.Profiles[0]
	setSubs(&cfg, p, Subscription{ID: "aaaaaaaaaaaaaaaa", Name: "经代理", Enabled: true,
		URL: "http://upstream.test/sub", UnlockURL: "http://upstream.test/unlock"})
	res, err := Build(cfg, *p, NewFetcher(t.TempDir()), "", false)
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

func TestProxySummary(t *testing.T) {
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(k, "")
	}
	if s := proxySummary(); !strings.Contains(s, "直连") {
		t.Errorf("未设置代理: %s", s)
	}
	t.Setenv("HTTPS_PROXY", "socks5://user:secret@10.0.0.1:1080")
	t.Setenv("NO_PROXY", "localhost,.lan")
	s := proxySummary()
	if strings.Contains(s, "secret") || !strings.Contains(s, "HTTPS_PROXY=socks5://user:xxxxx@10.0.0.1:1080") || !strings.Contains(s, "NO_PROXY=localhost,.lan") {
		t.Errorf("代理说明不符: %s", s)
	}
}

func TestMigrateLegacyConfig(t *testing.T) {
	open := func(old string) Config {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(old), 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := OpenStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		if !s.HasPassword() {
			t.Errorf("迁移后密码丢失")
		}
		if _, err := os.Stat(filepath.Join(dir, "config.json.bak")); err != nil {
			t.Errorf("没有留下迁移前的备份")
		}
		s2, err := OpenStore(dir) // 重新打开不应再次迁移
		if err != nil || !reflect.DeepEqual(s.Config(), s2.Config()) {
			t.Errorf("迁移结果不稳定: %v", err)
		}
		return s.Config()
	}
	const shared = `"groups":[{"name":"G","type":"select","extra":["DIRECT"],"nodes":"all"}],"rulesBefore":"DOMAIN,a.com,G",
		"ruleSets":[{"name":"R","url":"https://r.example/r.yaml","policy":"G","enabled":true}],"rulesAfter":"MATCH,G","parser":"yaml: {}","baseConfig":"mode: rule"`
	check := func(c Config, n int) {
		t.Helper()
		if c.Version != configVersion || len(c.Profiles) != n || c.LegacyGroups != nil || c.LegacyParser != "" || c.LegacyToken != "" {
			t.Fatalf("结构不符: %+v", c)
		}
		for _, p := range c.Profiles {
			if p.LegacySubscriptions != nil {
				t.Errorf("「%s」的旧订阅没有被移除", p.Name)
			}
			if len(p.Groups) != 1 || p.Groups[0].Name != "G" || len(p.RuleSets) != 1 || p.RulesBefore != "DOMAIN,a.com,G" || p.RulesAfter != "MATCH,G" {
				t.Errorf("「%s」没有拿到共用的策略组和规则: %+v", p.Name, p)
			}
		}
		if len(c.RuleSources) != len(seedRuleSources()) {
			t.Errorf("旧配置没有补上内置的规则来源: %+v", c.RuleSources)
		}
		last := c.Templates[len(c.Templates)-1]
		if last.Kind != "parser" || last.Parser != "yaml: {}" {
			t.Errorf("通用 Parser 没有存为模板: %+v", last)
		}
	}

	// v1：单订阅组
	c := open(`{"auth":{"hash":"h","salt":"s"},"config":{"token":"tok123","fileName":"old","cacheMinutes":30,
		"subscriptions":[{"id":"aaaaaaaaaaaaaaaa","name":"A","url":"https://a.example/s","enabled":true}],
		"customProxies":"- {name: x, type: http, server: 1.1.1.1, port: 80}",` + shared + `}}`)
	check(c, 1)
	p := c.Profiles[0]
	if p.Token != "tok123" || p.FileName != "old" || len(p.Sources) != 2 || len(c.Sources) != 2 || p.Parser != "yaml: {}" ||
		c.Sources[0].ID != "aaaaaaaaaaaaaaaa" || c.Sources[0].URL != "https://a.example/s" || c.Sources[0].UAMode != "fixed" ||
		p.Sources[0].Source != "aaaaaaaaaaaaaaaa" || !p.Sources[0].Enabled ||
		c.Sources[1].Kind != "manual" || !strings.Contains(c.Sources[1].Nodes, "name: x") || p.Sources[1].Source != c.Sources[1].ID {
		t.Errorf("v1 迁移结果不符: %+v / %+v", p, c.Sources)
	}

	// v2：多订阅组，策略组和规则共用
	c = open(`{"auth":{"hash":"h","salt":"s"},"config":{"profiles":[
		{"id":"1111111111111111","name":"P1","token":"t1","subscriptions":[],"customProxies":"","parser":"commands: []"},
		{"id":"2222222222222222","name":"P2","token":"t2","subscriptions":[],"customProxies":"ss://YWVzLTI1Ni1nY206cGFzcw@1.2.3.4:8388#n","parser":""}],` + shared + `}}`)
	check(c, 2)
	if c.Profiles[0].Token != "t1" || c.Profiles[0].Parser != "commands: []" || c.Profiles[1].Parser != "yaml: {}" ||
		len(c.Profiles[0].Sources) != 0 || len(c.Profiles[1].Sources) != 1 || len(c.Sources) != 1 || c.BaseConfig != "mode: rule" {
		t.Errorf("v2 迁移结果不符: %+v", c.Profiles)
	}
	c.Profiles[0].Groups[0].Name = "改" // 各组拿到的是独立副本
	if c.Profiles[1].Groups[0].Name != "G" {
		t.Errorf("订阅组之间共享了同一份策略组")
	}

	// v3：专用链接被移除，默认链接为空的订阅改用第一条有链接的专用链接；
	// 各订阅的固定 UA 挪到全局（取第一个填了的）；其余内容原样保留
	const v3 = `{"auth":{"hash":"h","salt":"s"},"config":{"version":3,"baseConfig":"mode: rule",
		"templates":[{"id":"3333333333333333","name":"T","kind":"parser","parser":"yaml: {}"}],
		"profiles":[{"id":"1111111111111111","name":"P1","token":"t1","rulesAfter":"MATCH,DIRECT","subscriptions":[
			{"id":"aaaaaaaaaaaaaaaa","name":"A","url":"https://a.example/s","enabled":true,"userAgent":"","variants":[{"match":"stash","url":"https://a.example/stash"}]},
			{"id":"bbbbbbbbbbbbbbbb","name":"B","url":" ","enabled":true,"userAgent":"UA_B","variants":[{"match":"x","url":""},{"match":"stash","url":"https://b.example/stash"},{"match":"loon","url":"https://b.example/loon"}]},
			{"id":"cccccccccccccccc","kind":"manual","name":"M","enabled":true,"userAgent":"other/2","nodes":"- {name: x, type: http, server: 1.1.1.1, port: 80}","variants":[]}]}]}}`
	c = open(strings.ReplaceAll(v3, "UA_B", "mihomo/1.19"))
	p = c.Profiles[0]
	if c.Version != configVersion || len(c.Templates) != 1 || p.Token != "t1" || p.RulesAfter != "MATCH,DIRECT" || len(p.Sources) != 3 || len(c.Sources) != 3 {
		t.Fatalf("v3 迁移后结构不符: %+v", c)
	}
	if c.Sources[0].URL != "https://a.example/s" || c.Sources[1].URL != "https://b.example/stash" {
		t.Errorf("v3 迁移后的订阅链接不符: %+v", c.Sources)
	}
	if c.UserAgent != "mihomo/1.19" || c.UnlockUserAgent != "" {
		t.Errorf("v3 迁移后的全局 UA 不符: %q / %q", c.UserAgent, c.UnlockUserAgent)
	}
	// 订阅里填的就是内置默认 UA 时，全局留空（继续跟随默认值）
	if c = open(strings.ReplaceAll(v3, "UA_B", defaultUA)); c.UserAgent != "" {
		t.Errorf("默认 UA 不应写进全局设置: %q", c.UserAgent)
	}
}

// v4 → v5：各订阅组里的订阅提成全局来源，内容相同的合并，过滤和前缀留在订阅组的引用上
func TestMigrateSharedSources(t *testing.T) {
	dir := t.TempDir()
	const v4 = `{"auth":{"hash":"h","salt":"s"},"config":{"version":4,"baseConfig":"mode: rule","userAgent":"fixed/1","templates":[],
		"profiles":[
		{"id":"1111111111111111","name":"P1","token":"t1","subscriptions":[
			{"id":"aaaaaaaaaaaaaaaa","name":"机场A","url":"https://a.example/s","uaMode":"passthrough","unlockUrl":"https://a.example/u","enabled":true,"prefix":"[1] ","include":"香港"},
			{"id":"bbbbbbbbbbbbbbbb","kind":"manual","name":"自建","enabled":true,"nodes":"- {name: x, type: http, server: 1.1.1.1, port: 80}"},
			{"id":"cccccccccccccccc","name":"没填","url":"","enabled":false}]},
		{"id":"2222222222222222","name":"P2","token":"t2","subscriptions":[
			{"id":"dddddddddddddddd","name":"A 的另一个名字","url":" https://a.example/s ","uaMode":"passthrough","unlockUrl":"https://a.example/u","enabled":false,"exclude":"过期"},
			{"id":"eeeeeeeeeeeeeeee","name":"固定 UA 的 A","url":"https://a.example/s","uaMode":"fixed","unlockUrl":"https://a.example/u","enabled":true},
			{"id":"ffffffffffffffff","kind":"manual","name":"自建 2","enabled":true,"nodes":"- {name: x, type: http, server: 1.1.1.1, port: 80}"},
			{"id":"aaaaaaaaaaaaaaaa","name":"没填 2","url":"","enabled":true}]}]}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(v4), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := s.Config()
	var names []string
	for _, src := range c.Sources {
		names = append(names, src.Name)
	}
	// P2 的第一条和 P1 的机场A 完全相同 → 合并；UA 模式不同的不合并；没填链接的各自保留（重复的 ID 重新生成）
	if want := []string{"机场A", "自建", "没填", "固定 UA 的 A", "没填 2"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("来源 = %v", names)
	}
	a, manual := c.Sources[0], c.Sources[1]
	if a.ID != "aaaaaaaaaaaaaaaa" || a.URL != "https://a.example/s" || a.UAMode != "passthrough" || a.UnlockURL != "https://a.example/u" || manual.Kind != "manual" {
		t.Errorf("来源内容不符: %+v", c.Sources)
	}
	p1, p2 := c.Profiles[0], c.Profiles[1]
	if !reflect.DeepEqual(p1.Sources, []SourceRef{
		{Source: a.ID, Enabled: true, Prefix: "[1] ", Include: "香港"}, {Source: manual.ID, Enabled: true}, {Source: c.Sources[2].ID},
	}) {
		t.Errorf("P1 的引用不符: %+v", p1.Sources)
	}
	if !reflect.DeepEqual(p2.Sources, []SourceRef{
		{Source: a.ID, Exclude: "过期"}, {Source: c.Sources[3].ID, Enabled: true}, {Source: manual.ID, Enabled: true}, {Source: c.Sources[4].ID, Enabled: true},
	}) {
		t.Errorf("P2 的引用不符: %+v", p2.Sources)
	}
	if id := c.Sources[4].ID; id == "aaaaaaaaaaaaaaaa" || !validID(id) {
		t.Errorf("重复的来源 ID 没有重新生成: %q", id)
	}
	if p1.Token != "t1" || p2.Token != "t2" || c.UserAgent != "fixed/1" {
		t.Errorf("其余内容应原样保留: %+v", c)
	}
	// 迁移结果能通过保存时的校验，且校验不改变它
	before := s.Config()
	if err := validateConfig(&c, map[string]string{p1.ID: "t1", p2.ID: "t2"}); err != nil || !reflect.DeepEqual(before.Sources, c.Sources) ||
		!reflect.DeepEqual(before.Profiles[0].Sources, c.Profiles[0].Sources) || !reflect.DeepEqual(before.Profiles[1].Sources, c.Profiles[1].Sources) {
		t.Errorf("迁移结果过不了校验或被校验改动: %v", err)
	}
}

func TestValidateSourcesAndRefs(t *testing.T) {
	cfg := defaultConfig()
	tplGroups := cfg.Templates[0].ID
	cfg.Sources = []Source{
		{ID: "aaaaaaaaaaaaaaaa", Name: "A", URL: "https://a.example/s", UAMode: "passthrough"},
		{ID: "not-a-valid-id", Name: "B", Kind: "manual", Nodes: "- {name: x, type: http, server: 1.1.1.1, port: 80}", URL: "https://leftover.example"},
		{ID: "aaaaaaaaaaaaaaaa", Name: "和 A 撞 ID"},
	}
	p := &cfg.Profiles[0]
	p.Sources = []SourceRef{{Source: "aaaaaaaaaaaaaaaa", Enabled: true}, {Source: "not-a-valid-id", Prefix: "[B] "}, {Source: "gone"}, {Source: ""}}
	p.Applied = map[string]TemplateRef{
		"groups": {ID: tplGroups, Hash: "abc"}, // 保留
		"rules":  {ID: tplGroups, Hash: "abc"}, // 类型对不上
		"parser": {ID: "0000000000000000", Hash: "abc"},
	}
	if err := validateConfig(&cfg, map[string]string{p.ID: "tok"}); err != nil {
		t.Fatal(err)
	}
	p = &cfg.Profiles[0]
	b := cfg.Sources[1]
	if !validID(b.ID) || b.URL != "" || cfg.Sources[2].ID == "aaaaaaaaaaaaaaaa" || cfg.Sources[2].Kind != "remote" || cfg.Sources[2].UAMode != "fixed" {
		t.Errorf("来源没有被规范化: %+v", cfg.Sources)
	}
	// 悬空的引用被去掉；ID 被重新生成的来源，引用跟着改
	if want := []SourceRef{{Source: "aaaaaaaaaaaaaaaa", Enabled: true}, {Source: b.ID, Prefix: "[B] "}}; !reflect.DeepEqual(p.Sources, want) {
		t.Errorf("引用 = %+v", p.Sources)
	}
	if want := map[string]TemplateRef{"groups": {ID: tplGroups, Hash: "abc"}}; !reflect.DeepEqual(p.Applied, want) || p.Token != "tok" {
		t.Errorf("套用记录 = %+v", p.Applied)
	}

	cfg.Sources[1].Nodes = "这不是节点"
	if err := validateConfig(&cfg, nil); err == nil || !strings.Contains(err.Error(), "B") {
		t.Errorf("手动节点格式错误应当报错: %v", err)
	}
}

func TestSeedTemplatesAreConsistent(t *testing.T) {
	// 任意策略组模板 + 任意规则模板组合，都不应出现「引用了不存在的策略」
	var groups, rules []Template
	for _, tpl := range seedTemplates() {
		switch tpl.Kind {
		case "groups":
			groups = append(groups, tpl)
		case "rules":
			rules = append(rules, tpl)
		}
	}
	for _, g := range groups {
		for _, r := range rules {
			cfg := defaultConfig()
			p := cfg.Profiles[0]
			p.Groups, p.Rules = g.Groups, r.Rules
			setSubs(&cfg, &p, Subscription{ID: "aaaaaaaaaaaaaaaa", Kind: "manual", Enabled: true,
				Nodes: "- {name: 🇭🇰 HK 01, type: http, server: 1.1.1.1, port: 80}\n- {name: US-2, type: http, server: 1.1.1.2, port: 80}\n- {name: 日本 3, type: http, server: 1.1.1.3, port: 80}\n- {name: Singapore 4, type: http, server: 1.1.1.4, port: 80}"})
			res, err := Build(cfg, p, NewFetcher(t.TempDir()), "", false)
			if err != nil || len(res.Warnings) > 0 {
				t.Errorf("%s + %s: err=%v warnings=%v", g.Name, r.Name, err, res.Warnings)
			}
		}
	}
}

// 前置代理（代理链）：引用上填了 Dialer，这批节点都带上 dialer-proxy；目标不存在、或能绕回节点自己时给出警告。
func TestDialerProxy(t *testing.T) {
	cfg := defaultConfig()
	p := &cfg.Profiles[0]
	setSubs(&cfg, p,
		Subscription{ID: "aaaaaaaaaaaaaaaa", Kind: "manual", Name: "机场", Enabled: true,
			Nodes: "- {name: 香港 01, type: ss, server: a.com, port: 443, cipher: aes-128-gcm, password: x}"},
		Subscription{ID: "bbbbbbbbbbbbbbbb", Kind: "manual", Name: "家宽", Enabled: true, Prefix: "家宽 ",
			Nodes: "- {name: 落地, type: http, server: 9.9.9.9, port: 8080, dialer-proxy: 旧值}"},
	)
	p.Rules = Rules{RulesAfter: "MATCH,全部"}
	p.Groups = []Group{
		{Name: "跳板", Type: "select", Nodes: "filter", Filter: "香港"},
		{Name: "全部", Type: "select", Extra: []string{"跳板"}, Nodes: "all"},
	}
	build := func(dialer string) (map[string]any, []string) {
		t.Helper()
		p.Sources[1].Dialer = dialer
		res, err := Build(cfg, *p, NewFetcher(t.TempDir()), "", false)
		if err != nil {
			t.Fatal(err)
		}
		var root map[string]any
		if err := yaml.Unmarshal(res.YAML, &root); err != nil {
			t.Fatal(err)
		}
		nodes := map[string]any{}
		for _, n := range root["proxies"].([]any) {
			nodes[fmt.Sprint(n.(map[string]any)["name"])] = n.(map[string]any)["dialer-proxy"]
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
