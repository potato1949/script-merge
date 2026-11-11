package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// 保存时要带上加载时的版本号：对不上就拒绝，除非明确要求覆盖。
func TestSaveConflict(t *testing.T) {
	a, do := newTestApp(t)
	put := func(c Config, query string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(c)
		return do("PUT", "/api/config"+query, string(b))
	}
	first := a.store.Config()
	first.Profiles[0].Name = "甲"
	saved := decode[Config](t, put(first, ""))
	if saved.Revision != first.Revision+1 {
		t.Fatalf("保存后版本号应加一: %d → %d", first.Revision, saved.Revision)
	}
	// 另一个页面还拿着旧的那份
	first.Profiles[0].Name = "乙"
	if w := put(first, ""); w.Code != http.StatusConflict {
		t.Fatalf("拿着旧版本保存应被拒绝: HTTP %d %s", w.Code, w.Body)
	}
	if got := a.store.Config().Profiles[0].Name; got != "甲" {
		t.Errorf("被拒绝的保存不应生效: %q", got)
	}
	if got := decode[Config](t, put(first, "?force=1")); got.Profiles[0].Name != "乙" || got.Revision != saved.Revision+1 {
		t.Errorf("强制覆盖: %q，版本 %d", got.Profiles[0].Name, got.Revision)
	}
}

// 每次保存有变化时留下被替换的那份，只留最近 maxHistory 份；恢复后令牌和内容都回到当时。
func TestHistory(t *testing.T) {
	a, do := newTestApp(t)
	history := func() []Snapshot {
		t.Helper()
		return decode[struct{ Items []Snapshot }](t, do("GET", "/api/history", "")).Items
	}
	save := func(name string) Config {
		t.Helper()
		c := a.store.Config()
		c.Profiles[0].Name = name
		b, _ := json.Marshal(c)
		return decode[Config](t, do("PUT", "/api/config", string(b)))
	}
	if n := len(history()); n != 0 {
		t.Fatalf("新安装不应有历史版本: %d", n)
	}
	token := save("第 0 版").Profiles[0].Token
	save("第 0 版") // 内容没变，不留历史
	if n := len(history()); n != 1 {
		t.Fatalf("内容没变的保存不应产生历史版本: %d", n)
	}
	for i := 1; i <= maxHistory+3; i++ {
		save(fmt.Sprintf("第 %d 版", i))
	}
	list := history()
	if len(list) != maxHistory {
		t.Fatalf("应只保留 %d 份: %d", maxHistory, len(list))
	}
	// 最新的一份历史是最后一次保存所替换掉的
	if want := fmt.Sprintf("第 %d 版", maxHistory+2); list[0].Profiles[0] != want {
		t.Errorf("最新的历史版本: %v，期望 %s", list[0].Profiles, want)
	}
	decode[Config](t, do("POST", "/api/token", `{"profile":"`+a.store.Config().Profiles[0].ID+`"}`))
	if a.store.Config().Profiles[0].Token == token {
		t.Fatal("令牌没有被重置")
	}
	target := history()[3]
	got := decode[Config](t, do("POST", "/api/history", `{"id":"`+target.ID+`"}`))
	if got.Profiles[0].Name != target.Profiles[0] || got.Profiles[0].Token != token {
		t.Errorf("恢复后: 名称 %q（期望 %q），令牌是否回到当时: %v", got.Profiles[0].Name, target.Profiles[0], got.Profiles[0].Token == token)
	}
	if w := do("POST", "/api/history", `{"id":"../config"}`); w.Code != http.StatusNotFound {
		t.Errorf("不在列表里的 ID 应被拒绝: HTTP %d", w.Code)
	}
}

// 预览用的是请求里带来的配置，不保存；来源的拉取结果能从状态接口看到。
func TestPreviewUnsavedAndSourceStatus(t *testing.T) {
	var fail atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Subscription-Userinfo", "upload=1; download=2; total=10; expire=4102444800")
		fmt.Fprint(w, "proxies:\n  - {name: a, type: http, server: 1.1.1.1, port: 80}\n  - {name: b, type: http, server: 1.1.1.2, port: 80}\n")
	}))
	defer up.Close()

	a, do := newTestApp(t)
	before := a.store.Config()
	draft := a.store.Config()
	const id = "bbbbbbbbbbbbbbbb"
	draft.Sources = []Source{{ID: id, Name: "机场", URL: up.URL}}
	draft.Profiles = append(draft.Profiles, Profile{Name: "还没保存的组", Sources: []SourceRef{{Source: id, Enabled: true, Exclude: "b"}}})
	preview := func(refresh bool) struct {
		YAML  string
		Total int
		Subs  []SubStatus
	} {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"config": draft, "profile": len(draft.Profiles) - 1, "refresh": refresh})
		return decode[struct {
			YAML  string
			Total int
			Subs  []SubStatus
		}](t, do("POST", "/api/preview", string(b)))
	}
	res := preview(false)
	if res.Total != 1 || !strings.Contains(res.YAML, "name: a") || strings.Contains(res.YAML, "name: b") {
		t.Errorf("预览应按未保存的配置生成: 共 %d 个节点\n%s", res.Total, res.YAML)
	}
	after := a.store.Config()
	if len(after.Profiles) != len(before.Profiles) || len(after.Sources) != len(before.Sources) || after.Revision != before.Revision {
		t.Errorf("预览不应保存配置: %d 个订阅组、%d 个来源、版本 %d", len(after.Profiles), len(after.Sources), after.Revision)
	}
	if n := len(a.store.History()); n != 0 {
		t.Errorf("预览不应产生历史版本: %d", n)
	}

	status := func() SourceState {
		t.Helper()
		return decode[map[string]SourceState](t, do("GET", "/api/source-status", ""))[id]
	}
	ok := status()
	if ok.Error != "" || ok.Count != 2 || ok.OKTime.IsZero() || !strings.Contains(ok.Userinfo, "total=10") {
		t.Errorf("成功后的状态: %+v", ok)
	}
	fail.Store(true)
	if res := preview(true); len(res.Subs) != 1 || res.Subs[0].Error == "" || !res.Subs[0].Stale {
		t.Errorf("上游失败时应回退到旧缓存并带上错误: %+v", res.Subs)
	}
	bad := status()
	if bad.Error == "" || bad.Count != 2 || !bad.OKTime.Equal(ok.OKTime) || !bad.Time.After(ok.OKTime) {
		t.Errorf("失败后应保留上次成功的信息并记下错误: %+v", bad)
	}
	// 重启后从磁盘缓存恢复成功的那部分
	if st := NewFetcher(filepath.Dir(a.fetcher.dir)).States()[id]; st.Count != 2 || st.Error != "" {
		t.Errorf("重启后的状态: %+v", st)
	}
}
