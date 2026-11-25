package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// 新增的策略组：成员是选中的底板策略组加上节点名匹配正则的节点，追加在底板的策略组之后、Parser 之前。
func TestAddGroups(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `proxies:
  - {name: 香港 01, type: ss, server: a.com, port: 443, cipher: aes-128-gcm, password: x}
  - {name: 日本 01, type: ss, server: b.com, port: 443, cipher: aes-128-gcm, password: x}
proxy-groups:
  - {name: 全部, type: select, proxies: [香港 01, 日本 01]}
  - {name: 自动, type: url-test, url: 'http://t', interval: 300, proxies: [香港 01, 日本 01]}
rules:
  - MATCH,全部
`)
	}))
	defer srv.Close()
	cfg := testConfig()
	p := &cfg.Profiles[0]
	addSource(&cfg, p, Source{ID: "aaaaaaaaaaaaaaaa", Name: "机场", URL: srv.URL}, SourceRef{Enabled: true})
	addSource(&cfg, p, Source{ID: "bbbbbbbbbbbbbbbb", Kind: "manual", Name: "家宽",
		Nodes: "- {name: 落地 A, type: http, server: 9.9.9.9, port: 8080}\n- {name: 落地 B, type: http, server: 9.9.9.8, port: 8080}"},
		SourceRef{Enabled: true, Prefix: "家宽 "})
	p.Base = "aaaaaaaaaaaaaaaa"
	p.Groups = []ProxyGroup{
		{ID: "1111111111111111", Name: "自建", Groups: []string{"自动", "改了名的组", "全部"}, Filter: "^家宽", Icon: " https://icons.example/home.png "},
		{ID: "2222222222222222", Name: "全部", Filter: "."},
		{ID: "3333333333333333", Name: "香港 01"},
		{ID: "4444444444444444", Name: "正则写错", Filter: "("},
		{ID: "5555555555555555", Name: "只要节点", Filter: "01$"},
	}
	p.Parser = "yaml:\n  commands:\n    - proxy-groups.(全部).proxies+自建\n"
	if err := validateConfig(&cfg, nil); err != nil {
		t.Fatal(err)
	}
	res, err := Build(cfg, cfg.Profiles[0], NewFetcher(t.TempDir()), FetchAuto)
	if err != nil {
		t.Fatal(err)
	}
	out := parseOut(t, res)
	groups := map[string][]string{}
	var names []string
	for _, g := range out.Groups {
		groups[g.Name] = g.Proxies
		names = append(names, g.Name)
	}
	// 追加在末尾，重名的跳过；底板的「全部」没被覆盖，Parser 能引用新增的组
	if want := []string{"全部", "自动", "自建", "正则写错", "只要节点"}; !reflect.DeepEqual(names, want) {
		t.Errorf("策略组 = %q", names)
	}
	if want := []string{"香港 01", "日本 01", "家宽 落地 A", "家宽 落地 B", "自建"}; !reflect.DeepEqual(groups["全部"], want) {
		t.Errorf("全部 = %q", groups["全部"])
	}
	// 底板策略组按选择的顺序在前，节点按输出顺序在后
	if want := []string{"自动", "全部", "家宽 落地 A", "家宽 落地 B"}; !reflect.DeepEqual(groups["自建"], want) {
		t.Errorf("自建 = %q", groups["自建"])
	}
	// 填了图标链接的输出 icon（保存时去掉首尾空白），没填的不输出
	if !strings.Contains(string(res.YAML), "icon: https://icons.example/home.png") || strings.Count(string(res.YAML), "icon:") != 1 {
		t.Errorf("图标链接输出不对:\n%s", res.YAML)
	}
	if want := []string{"DIRECT"}; !reflect.DeepEqual(groups["正则写错"], want) {
		t.Errorf("正则写错 = %q", groups["正则写错"])
	}
	if want := []string{"香港 01", "日本 01"}; !reflect.DeepEqual(groups["只要节点"], want) {
		t.Errorf("只要节点 = %q", groups["只要节点"])
	}
	for _, w := range []string{"「改了名的组」不存在", "「全部」和底板的策略组重名", "「香港 01」和节点重名", "「正则写错」的节点正则", "「正则写错」没有成员"} {
		if !strings.Contains(strings.Join(res.Warnings, "\n"), w) {
			t.Errorf("缺少警告 %q：%q", w, res.Warnings)
		}
	}
	if len(res.Warnings) != 5 {
		t.Errorf("警告 = %q", res.Warnings)
	}
	if len(res.NewGroups) != 5 || res.NewGroups[0].ID != "1111111111111111" || res.NewGroups[0].Groups != 2 ||
		res.NewGroups[1].Skipped == "" || res.NewGroups[2].Skipped == "" || res.Groups != 5 {
		t.Errorf("NewGroups = %+v，策略组数 = %d", res.NewGroups, res.Groups)
	}
}

func TestNormalizeGroups(t *testing.T) {
	check := func(groups []ProxyGroup, wantErr string) *Profile {
		t.Helper()
		cfg := testConfig()
		cfg.Profiles[0].Groups = groups
		err := validateConfig(&cfg, nil)
		if (err == nil) != (wantErr == "") || (err != nil && !strings.Contains(err.Error(), wantErr)) {
			t.Errorf("错误 = %v，应为 %q", err, wantErr)
		}
		return &cfg.Profiles[0]
	}
	if p := check(nil, ""); p.Groups == nil {
		t.Error("Groups 应规范化为空数组")
	}
	p := check([]ProxyGroup{
		{ID: "bad", Name: " A ", Groups: []string{" x ", "", "x", "y"}},
		{ID: "bad", Name: "B"},
	}, "")
	if g := p.Groups[0]; g.Name != "A" || !validID(g.ID) || !reflect.DeepEqual(g.Groups, []string{"x", "y"}) {
		t.Errorf("没有规范化: %+v", g)
	}
	if p.Groups[0].ID == p.Groups[1].ID {
		t.Error("重复的 ID 应重新生成")
	}
	check([]ProxyGroup{{Name: " "}}, "没有填名称")
	check([]ProxyGroup{{Name: "DIRECT"}}, "不能叫「DIRECT」")
	check([]ProxyGroup{{Name: "A"}, {Name: "A "}}, "重名")
	check([]ProxyGroup{{Name: "A", Icon: "icons/a.png"}}, "图标链接必须以 http:// 或 https:// 开头")
	if p := check([]ProxyGroup{{Name: "A", Icon: " https://a.example/a.png "}}, ""); p.Groups[0].Icon != "https://a.example/a.png" {
		t.Errorf("图标链接应去掉首尾空白: %q", p.Groups[0].Icon)
	}
}
