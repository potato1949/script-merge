package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"
)

func TestAccessLog(t *testing.T) {
	a, do := newTestApp(t)
	cfg := a.store.Config()
	cfg.Sources = []Source{{ID: "aaaaaaaaaaaaaaaa", Name: "自建", Kind: "manual", Nodes: "- {name: x, type: http, server: 1.1.1.1, port: 80}"}}
	cfg.Profiles[0].Sources = []SourceRef{{Source: "aaaaaaaaaaaaaaaa", Enabled: true}}
	cfg.Profiles = append(cfg.Profiles, Profile{Name: "空的"})
	body, _ := json.Marshal(cfg)
	cfg = decode[Config](t, do("PUT", "/api/config", string(body)))
	p, empty := cfg.Profiles[0], cfg.Profiles[1]
	records := func(id string) []AccessRecord {
		t.Helper()
		return decode[struct{ Records []AccessRecord }](t, do("GET", "/api/access?profile="+id, "")).Records
	}

	from := func(remote, ua, xff string) func(*http.Request) {
		return func(r *http.Request) {
			r.RemoteAddr = remote
			r.Header.Set("User-Agent", ua)
			if xff != "" {
				r.Header.Set("X-Forwarded-For", xff)
			}
		}
	}
	if w := do("GET", "/sub/"+p.Token, "", from("198.51.100.7:4000", "mihomo/1.19", "6.6.6.6")); w.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body)
	}
	do("GET", "/sub/"+p.Token, "", from("127.0.0.1:4000", "Stash/3", "6.6.6.6, 203.0.113.9, 10.0.0.2")) // 经本机反代
	do("GET", "/sub/"+p.Token, "", from("198.51.100.7:4001", "mihomo/1.19", ""))
	if w := do("GET", "/sub/"+empty.Token, "", from("198.51.100.7:4002", "x", "")); w.Code != http.StatusBadGateway {
		t.Errorf("没有节点的订阅组: HTTP %d", w.Code)
	}
	do("GET", "/sub/no-such-token", "") // 令牌不对的请求不属于任何订阅组，不记录
	do("GET", "/api/preview?profile="+p.ID, "")

	got := records(p.ID)
	for i := range got {
		if got[i].Time.IsZero() {
			t.Errorf("记录没有时间: %+v", got[i])
		}
		got[i].Time = time.Time{}
	}
	want := []AccessRecord{ // 最新的在前
		{IP: "198.51.100.7", UA: "mihomo/1.19", Nodes: 1, Status: 200},
		{IP: "203.0.113.9", UA: "Stash/3", Nodes: 1, Status: 200, NewIP: true},
		{IP: "198.51.100.7", UA: "mihomo/1.19", Nodes: 1, Status: 200}, // 直连的请求不看转发头；第一个 IP 不算新 IP
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("记录 = %+v", got)
	}
	if got := records(empty.ID); len(got) != 1 || got[0].Status != http.StatusBadGateway || got[0].Nodes != 0 {
		t.Errorf("失败的拉取也应记录: %+v", got)
	}

	// 记录写在磁盘上，重启后还在
	if again := OpenAccessLog(filepath.Dir(a.access.path), 0).Get(p.ID); len(again) != 3 || again[1].IP != "203.0.113.9" {
		t.Errorf("重新打开后的记录 = %+v", again)
	}
	// 只保留最近 maxAccessRecords 条
	for i := 0; i < maxAccessRecords+5; i++ {
		a.access.Add(p.ID, AccessRecord{Time: time.Now(), IP: "198.51.100.7", UA: strconv.Itoa(i)})
	}
	if got := records(p.ID); len(got) != maxAccessRecords || got[0].UA != strconv.Itoa(maxAccessRecords+4) {
		t.Errorf("记录数 = %d", len(got))
	}

	// 删除订阅组后它的记录一并清掉；清空接口只清指定的订阅组
	cfg.Profiles = cfg.Profiles[:1]
	body, _ = json.Marshal(cfg)
	decode[Config](t, do("PUT", "/api/config", string(body)))
	if len(a.access.Get(empty.ID)) != 0 || len(a.access.Get(p.ID)) == 0 {
		t.Errorf("删除订阅组后记录没有按订阅组清理")
	}
	if w := do("DELETE", "/api/access?profile="+p.ID, ""); w.Code != http.StatusOK || len(records(p.ID)) != 0 {
		t.Errorf("清空失败: HTTP %d", w.Code)
	}
}

func TestClientIP(t *testing.T) {
	cases := []struct{ remote, xff, real, want string }{
		{"198.51.100.7:1", "1.2.3.4", "1.2.3.4", "198.51.100.7"},         // 公网直连：转发头可伪造，不看
		{"127.0.0.1:1", "1.2.3.4", "", "1.2.3.4"},                        // 本机反代
		{"172.17.0.1:1", "9.9.9.9, 1.2.3.4, 192.168.1.2", "", "1.2.3.4"}, // 取最右的公网地址，左边可伪造
		{"10.0.0.1:1", "192.168.1.50", "", "192.168.1.50"},               // 内网客户端经反代
		{"[::1]:1", "", "2001:db8::1", "2001:db8::1"},                    // 只有 X-Real-IP
		{"127.0.0.1:1", "garbage", "", "127.0.0.1"},
		{"192.168.1.9:1", "", "", "192.168.1.9"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if c.real != "" {
			r.Header.Set("X-Real-IP", c.real)
		}
		if got := clientIP(r); got != c.want {
			t.Errorf("%+v: got %s", c, got)
		}
	}
}
