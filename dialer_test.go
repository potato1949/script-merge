package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// upstreamServer 返回一个固定内容的假上游。
func upstreamServer(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
	t.Cleanup(srv.Close)
	return srv.URL
}

type builtOut struct {
	res     *BuildResult
	dialers map[string]any      // 节点名 → dialer-proxy
	groups  map[string][]string // 策略组名 → 成员
	order   []string            // 策略组的顺序
}

func buildOut(t *testing.T, cfg Config, p Profile, f *Fetcher) builtOut {
	t.Helper()
	res, err := Build(cfg, p, f, FetchAuto)
	if err != nil {
		t.Fatal(err)
	}
	out := parseOut(t, res)
	b := builtOut{res: res, dialers: map[string]any{}, groups: map[string][]string{}}
	for _, n := range out.Proxies {
		b.dialers[fmt.Sprint(n["name"])] = n["dialer-proxy"]
	}
	for _, g := range out.Groups {
		b.groups[g.Name] = g.Proxies
		b.order = append(b.order, g.Name)
	}
	return b
}

// 自动生成的前置组：成员是所有来源里输出名匹配正则、且不带前置代理的节点；落地节点写上 dialer-proxy 指向它。
func TestAutoDialer(t *testing.T) {
	base := upstreamServer(t, `proxies:
  - {name: 香港 01, type: ss, server: a.com, port: 443, cipher: aes-128-gcm, password: x}
  - {name: 日本 01, type: ss, server: b.com, port: 443, cipher: aes-128-gcm, password: x}
proxy-groups:
  - {name: 节点选择, type: select, proxies: [香港 01, 日本 01, DIRECT]}
rules:
  - MATCH,节点选择
`)
	other := upstreamServer(t, `proxies:
  - {name: 香港 02, type: ss, server: c.com, port: 443, cipher: aes-128-gcm, password: x}
  - {name: 美国 01, type: ss, server: d.com, port: 443, cipher: aes-128-gcm, password: x}
`)
	cfg := testConfig()
	p := &cfg.Profiles[0]
	addSource(&cfg, p, Source{ID: "aaaaaaaaaaaaaaaa", Name: "机场", URL: base}, SourceRef{Enabled: true})
	addSource(&cfg, p, Source{ID: "bbbbbbbbbbbbbbbb", Name: "机场B", URL: other}, SourceRef{Enabled: true, Prefix: "[B] "})
	// 落地节点自带的 dialer-proxy 会被覆盖
	addSource(&cfg, p, Source{ID: "cccccccccccccccc", Kind: "manual", Name: "家宽",
		Nodes: "- {name: 落地, type: http, server: 9.9.9.9, port: 8080, dialer-proxy: 旧值}\n- {name: 落地 香港, type: http, server: 9.9.9.8, port: 8080}"},
		SourceRef{Enabled: true, Prefix: "家宽 ", DialerAuto: true, DialerFilter: "香港", Dialer: "不会用到"})
	p.Base = "aaaaaaaaaaaaaaaa"
	f := NewFetcher(t.TempDir())

	b := buildOut(t, cfg, *p, f)
	// 「家宽 落地 香港」也匹配「香港」，但它自己带前置，必须剔除
	if want := []string{"香港 01", "[B] 香港 02"}; !reflect.DeepEqual(b.groups["家宽 前置"], want) {
		t.Errorf("前置组成员 = %v，应为 %v", b.groups["家宽 前置"], want)
	}
	if last := b.order[len(b.order)-1]; last != "家宽 前置" {
		t.Errorf("前置组应排在最后，实际最后一个是 %q", last)
	}
	for _, n := range []string{"家宽 落地", "家宽 落地 香港"} {
		if b.dialers[n] != "家宽 前置" {
			t.Errorf("%s 的 dialer-proxy = %v", n, b.dialers[n])
		}
	}
	for _, n := range []string{"香港 01", "[B] 香港 02", "[B] 美国 01"} {
		if b.dialers[n] != nil {
			t.Errorf("%s 不该有 dialer-proxy: %v", n, b.dialers[n])
		}
	}
	// 落地节点照常并进「节点选择」；前置组里没有它们，所以不成环
	if !strings.Contains(strings.Join(b.groups["节点选择"], ","), "家宽 落地") {
		t.Errorf("落地节点应并进节点选择: %v", b.groups["节点选择"])
	}
	if len(b.res.Warnings) != 0 || len(b.res.Loops) != 0 {
		t.Errorf("不该有警告或环: %v %v", b.res.Warnings, b.res.Loops)
	}
	st := b.res.Subs[2]
	if st.Dialer != "家宽 前置" || !reflect.DeepEqual(st.DialerMembers, []string{"香港 01", "[B] 香港 02"}) {
		t.Errorf("SubStatus 的前置信息 = %q %v", st.Dialer, st.DialerMembers)
	}

	// 正则为空：全部不带前置的节点；另一条引用填了前置（节点名），它的节点也要剔除
	p.Sources[2].DialerFilter = ""
	p.Sources[1].Dialer = "日本 01"
	b = buildOut(t, cfg, *p, f)
	if want := []string{"香港 01", "日本 01"}; !reflect.DeepEqual(b.groups["家宽 前置"], want) {
		t.Errorf("正则为空时前置组成员 = %v，应为 %v", b.groups["家宽 前置"], want)
	}
	if b.res.Subs[1].Dialer != "日本 01" || b.res.Subs[1].DialerMembers != nil {
		t.Errorf("填名字的引用 SubStatus = %q %v", b.res.Subs[1].Dialer, b.res.Subs[1].DialerMembers)
	}
	p.Sources[1].Dialer = ""

	// 正则写错 / 一个都匹配不到：填 REJECT（连不上），绝不能是 DIRECT
	for _, filter := range []string{"(", "不存在的地区"} {
		p.Sources[2].DialerFilter = filter
		b = buildOut(t, cfg, *p, f)
		if !reflect.DeepEqual(b.groups["家宽 前置"], []string{"REJECT"}) {
			t.Errorf("正则 %q：前置组成员 = %v，应为 [REJECT]", filter, b.groups["家宽 前置"])
		}
		if len(b.res.Warnings) != 1 || b.dialers["家宽 落地"] != "家宽 前置" {
			t.Errorf("正则 %q：警告 %v，dialer-proxy %v", filter, b.res.Warnings, b.dialers["家宽 落地"])
		}
	}
	p.Sources[2].DialerFilter = "香港"

	// 和新增的策略组重名：加序号，并且落地节点指向改名后的组
	p.Groups = []ProxyGroup{{ID: "dddddddddddddddd", Name: "家宽 前置", Groups: []string{"节点选择"}}}
	b = buildOut(t, cfg, *p, f)
	if b.dialers["家宽 落地"] != "家宽 前置 2" || len(b.groups["家宽 前置 2"]) != 2 || !reflect.DeepEqual(b.groups["家宽 前置"], []string{"节点选择"}) {
		t.Errorf("重名时：dialer-proxy %v，组 %v", b.dialers["家宽 落地"], b.groups)
	}
	if len(b.res.Warnings) != 1 || !strings.Contains(b.res.Warnings[0], "家宽 前置 2") {
		t.Errorf("重名应警告: %v", b.res.Warnings)
	}
	p.Groups = nil

	// 停用的引用不生成前置组
	p.Sources[2].Enabled = false
	b = buildOut(t, cfg, *p, f)
	if _, ok := b.groups["家宽 前置"]; ok {
		t.Error("停用的引用不该生成前置组")
	}
	p.Sources[2].Enabled = true

	// Parser 之后再把落地节点塞进前置组 → 成环要报出来
	p.Parser = "commands:\n  - proxy-groups.(家宽 前置).proxies+家宽 落地\n"
	b = buildOut(t, cfg, *p, f)
	if len(b.res.Loops) != 1 || b.res.Loops[0].Node != "家宽 落地" {
		t.Errorf("Parser 造成的环应报出: %v %v", b.res.Loops, b.res.Warnings)
	}
}

// 成环检查要按 mihomo 的规则展开 include-all / filter / exclude-filter：机场的地区组常这么写，
// 只看 proxies 会漏掉落地节点被自动选进去的情况。
func TestDialerLoopIncludeAll(t *testing.T) {
	root := func(group map[string]any) map[string]any {
		return map[string]any{
			"proxies": []any{
				map[string]any{"name": "香港 01", "type": "ss"},
				map[string]any{"name": "港 落地", "type": "http", "dialer-proxy": "香港"},
			},
			"proxy-groups": []any{group},
		}
	}
	known := map[string]bool{"香港 01": true, "港 落地": true, "香港": true}
	cases := []struct {
		name  string
		group map[string]any
		loop  bool
	}{
		{"只有显式成员", map[string]any{"name": "香港", "proxies": []any{"香港 01"}}, false},
		{"include-all 无 filter", map[string]any{"name": "香港", "include-all": true}, true},
		{"include-all-proxies + filter 匹配", map[string]any{"name": "香港", "include-all-proxies": true, "filter": "港"}, true},
		{"filter 多条正则", map[string]any{"name": "香港", "include-all": true, "filter": "日本`落地"}, true},
		{"filter 不匹配", map[string]any{"name": "香港", "include-all": true, "filter": "^香港"}, false},
		{"exclude-filter 排除", map[string]any{"name": "香港", "include-all": true, "filter": "港", "exclude-filter": "落地"}, false},
		{"filter 无效按全部匹配", map[string]any{"name": "香港", "include-all": true, "filter": "("}, true},
		{"exclude-filter 无效按不排除", map[string]any{"name": "香港", "include-all": true, "exclude-filter": "("}, true},
		{"只有 use 不影响", map[string]any{"name": "香港", "use": []any{"pp"}, "proxies": []any{"香港 01"}}, false},
		// exclude-filter 只作用于 include-all 选入的节点，显式列出的照样算（宁可多报）
		{"显式成员不受 exclude-filter 影响", map[string]any{"name": "香港", "proxies": []any{"港 落地"}, "exclude-filter": "落地"}, true},
	}
	for _, c := range cases {
		warns, loops := validateDialers(root(c.group), known)
		if got := len(loops) == 1; got != c.loop {
			t.Errorf("%s：成环 = %v，应为 %v（%v）", c.name, got, c.loop, warns)
			continue
		}
		if c.loop {
			if want := []string{"港 落地", "香港", "港 落地"}; !reflect.DeepEqual(loops[0].Path, want) {
				t.Errorf("%s：路径 = %v", c.name, loops[0].Path)
			}
			if len(warns) != 1 || !strings.Contains(warns[0], "港 落地 → 香港") {
				t.Errorf("%s：警告 = %v", c.name, warns)
			}
			if c.group["include-all"] == true && !strings.Contains(warns[0], "香港（include-all）") {
				t.Errorf("%s：警告里应标出 include-all：%v", c.name, warns)
			}
		}
	}

	// 经过多层：落地 → 前置组 → 另一个节点 → 它的前置 → 包含落地的组（include-all 选入）
	r := map[string]any{
		"proxies": []any{
			map[string]any{"name": "落地", "type": "http", "dialer-proxy": "A"},
			map[string]any{"name": "中转", "type": "http", "dialer-proxy": "B"},
		},
		"proxy-groups": []any{
			map[string]any{"name": "A", "proxies": []any{"中转"}},
			map[string]any{"name": "B", "include-all": true, "filter": "落地"},
		},
	}
	_, loops := validateDialers(r, map[string]bool{"落地": true, "中转": true, "A": true, "B": true})
	var paths []string
	for _, l := range loops {
		paths = append(paths, strings.Join(l.Path, " → "))
	}
	// 环上的两个节点都连不上，都要报
	if want := []string{"落地 → A → 中转 → B → 落地", "中转 → B → 落地 → A → 中转"}; !reflect.DeepEqual(paths, want) {
		t.Errorf("多层的环: %v", paths)
	}
}

// Parser 加的节点不经过前面的流程（不并进策略组、不匹配正则），哪里都用不到时提示改用手动节点来源。
func TestParserAddedNode(t *testing.T) {
	base := upstreamServer(t, `proxies:
  - {name: 香港 01, type: ss, server: a.com, port: 443, cipher: aes-128-gcm, password: x}
proxy-groups:
  - {name: 节点选择, type: select, proxies: [香港 01]}
rules:
  - MATCH,节点选择
`)
	cfg := testConfig()
	p := &cfg.Profiles[0]
	addSource(&cfg, p, Source{ID: "aaaaaaaaaaaaaaaa", Name: "机场", URL: base}, SourceRef{Enabled: true})
	p.Base = "aaaaaaaaaaaaaaaa"
	f := NewFetcher(t.TempDir())
	add := "append-proxies:\n  - {name: 新节点, type: http, server: 1.1.1.1, port: 80}\n"
	cases := []struct {
		parser string
		warn   bool
	}{
		{add, true},
		{add + "commands:\n  - proxy-groups.(节点选择).proxies+新节点\n", false},
		{add + "prepend-rules:\n  - DOMAIN,x.com,新节点\n", false},
		{add + "append-proxy-groups:\n  - {name: 全部, type: select, include-all: true}\n", false},
		// 改名也算：原名从组里消失，新名哪里都没有
		{"commands:\n  - proxies.(香港 01).name=香港 改\n", true},
	}
	for _, c := range cases {
		p.Parser = c.parser
		res, err := Build(cfg, *p, f, FetchAuto)
		if err != nil {
			t.Fatal(err)
		}
		got := false
		for _, w := range res.Warnings {
			got = got || strings.Contains(w, "手动节点来源")
		}
		if got != c.warn {
			t.Errorf("Parser %q：提示 = %v，应为 %v（%v）", c.parser, got, c.warn, res.Warnings)
		}
	}
}
