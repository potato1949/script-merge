package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
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
			fmt.Fprint(w, "proxies:\n  - {name: via-proxy, type: ss, server: a.com, port: 443}\nproxy-groups:\n  - {name: G, type: select, proxies: [via-proxy]}\n")
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

// addSource 给订阅组加一个来源和对它的引用。
func addSource(cfg *Config, p *Profile, s Source, r SourceRef) {
	if s.Kind == "" {
		s.Kind = "remote"
	}
	cfg.Sources = append(cfg.Sources, s)
	r.Source = s.ID
	p.Sources = append(p.Sources, r)
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
	up, err := parseSubscription([]byte(body))
	if err != nil || len(up.Proxies) != 5 {
		t.Fatalf("got %v, err %v", up, err)
	}
	ps := up.Proxies
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

// 格式化只统一缩进：注释、引号、流式写法保留，内容不变；YAML 有错时报错
func TestFormatParser(t *testing.T) {
	src := "# 注释\nyaml:\n    prepend-proxies:\n    -   name: Home   # 行尾\n        type: socks5\n    - {name: a, type: ss}\n    prepend-rules:\n      - 'DOMAIN,a.com,DIRECT'\n    commands:\n       - proxy-groups.(节点选择).proxies+[]proxyNames|家宽\n"
	want := "# 注释\nyaml:\n  prepend-proxies:\n    - name: Home # 行尾\n      type: socks5\n    - {name: a, type: ss}\n  prepend-rules:\n    - 'DOMAIN,a.com,DIRECT'\n  commands:\n    - proxy-groups.(节点选择).proxies+[]proxyNames|家宽\n"
	got, err := FormatParser(src)
	if err != nil || got != want {
		t.Fatalf("格式化结果 (%v):\n%s", err, got)
	}
	if again, _ := FormatParser(got); again != got {
		t.Errorf("再格式化一次应不变:\n%s", again)
	}
	if got, err := FormatParser("  \n"); got != "" || err != nil {
		t.Errorf("空内容 = %q, %v", got, err)
	}
	if _, err := FormatParser("yaml:\n  a: [1\n"); err == nil {
		t.Error("YAML 有错时应报错")
	}
}
