package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRelease 模拟 GitHub 的 releases/latest：跳转到 *tag 的 release 页面；tag 为空时返回 500。
func fakeRelease(t *testing.T, tag *string, mu *sync.Mutex) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		v := *tag
		mu.Unlock()
		if v == "" {
			http.Error(w, "rate limited", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/clash-verge-rev/clash-verge-rev/releases/tag/"+v, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestUAFollow(t *testing.T) {
	var mu sync.Mutex
	tag := "v2.5.10"
	set := func(v string) { mu.Lock(); tag = v; mu.Unlock() }
	srv := fakeRelease(t, &tag, &mu)

	dir := t.TempDir()
	f := NewFetcher(dir)
	f.UA.api = srv.URL
	cfg := Config{UserAgent: "manual/1.0"}
	ua := func() string { _, ua, _ := f.fetchArgs(cfg, Source{URL: "http://x"}); return ua }

	cfg.FollowUA = true
	if got := ua(); got != defaultUA { // 还没查到版本时用内置的，不用手填的
		t.Errorf("没查到版本时 UA = %q", got)
	}
	if v := f.UA.Check(); v.Version != "v2.5.10" || v.Error != "" {
		t.Fatalf("查询结果 %+v", v)
	}
	if got := ua(); got != "clash-verge/v2.5.10" {
		t.Errorf("跟随版本时 UA = %q", got)
	}
	cfg.FollowUA = false
	if got := ua(); got != "manual/1.0" {
		t.Errorf("关闭跟随后 UA = %q", got)
	}
	cfg.UserAgent = ""
	if got := ua(); got != defaultUA {
		t.Errorf("都没有时 UA = %q", got)
	}

	// 查到的版本落盘，重启后直接沿用
	if v := NewUAFollower(dir).State(); v.Version != "v2.5.10" {
		t.Errorf("重新打开后版本 = %+v", v)
	}

	// 失败或返回无法识别的版本号时保留之前的版本
	for _, bad := range []string{"", "autobuild", "v2.5.11-alpha"} {
		set(bad)
		if v := f.UA.Check(); v.Version != "v2.5.10" || v.Error == "" {
			t.Errorf("tag %q: %+v", bad, v)
		}
	}
	set("v2.6.0")
	if v := f.UA.Check(); v.Version != "v2.6.0" || v.Error != "" {
		t.Errorf("更新后 %+v", v)
	}
}

func TestUAVersionDue(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		v    UAVersion
		want bool
	}{
		{"从没查过", UAVersion{}, true},
		{"刚成功", UAVersion{Version: "v1.0.0", OKAt: now, CheckedAt: now}, false},
		{"成功已过期", UAVersion{Version: "v1.0.0", OKAt: now.Add(-uaCheckInterval), CheckedAt: now.Add(-uaCheckInterval)}, true},
		{"刚失败", UAVersion{Version: "v1.0.0", OKAt: now.Add(-2 * uaCheckInterval), CheckedAt: now, Error: "x"}, false},
		{"失败已久", UAVersion{OKAt: now.Add(-2 * uaCheckInterval), CheckedAt: now.Add(-uaRetryInterval), Error: "x"}, true},
	}
	for _, c := range cases {
		if got := c.v.due(now); got != c.want {
			t.Errorf("%s: due = %v", c.name, got)
		}
	}
}

// 开启跟随后，拉取上游用的是查到的版本；接口能查询和手动触发检查。
func TestUAFollowAPI(t *testing.T) {
	var mu sync.Mutex
	tag := "v2.5.10"
	srv := fakeRelease(t, &tag, &mu)
	a, do := newTestApp(t)
	a.fetcher.UA.api = srv.URL

	if v := decode[UAVersion](t, do("GET", "/api/ua-version", "")); v.Version != "" {
		t.Errorf("还没查过: %+v", v)
	}
	if v := decode[UAVersion](t, do("POST", "/api/ua-version", "")); v.Version != "v2.5.10" {
		t.Errorf("手动检查: %+v", v)
	}

	var gotUA string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotUA = r.UserAgent()
		mu.Unlock()
		fmt.Fprint(w, "proxies:\n  - {name: A, type: ss, server: a.com, port: 443}\nproxy-groups:\n  - {name: G, type: select, proxies: [A]}\n")
	}))
	defer up.Close()
	cfg := testConfig()
	cfg.FollowUA, cfg.UserAgent = true, "manual/1.0"
	addSource(&cfg, &cfg.Profiles[0], Source{ID: "aaaaaaaaaaaaaaaa", Name: "底板", URL: up.URL}, SourceRef{Enabled: true})
	b, _ := json.Marshal(map[string]any{"config": cfg, "profile": 0})
	decode[map[string]any](t, do("POST", "/api/preview", string(b)))
	mu.Lock()
	defer mu.Unlock()
	if !strings.HasPrefix(gotUA, "clash-verge/v2.5.10") {
		t.Errorf("上游收到的 UA = %q", gotUA)
	}
}
