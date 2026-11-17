package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestApp(t *testing.T) (a *App, do func(method, path, body string, edit ...func(*http.Request)) *httptest.ResponseRecorder) {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a = &App{store: store, fetcher: NewFetcher(dir), access: OpenAccessLog(dir, 0), sessions: map[string]time.Time{}}
	rec := httptest.NewRecorder()
	a.startSession(rec, httptest.NewRequest("GET", "/", nil))
	cookie := rec.Result().Cookies()[0]
	h := a.routes()
	return a, func(method, path, body string, edit ...func(*http.Request)) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.AddCookie(cookie)
		for _, f := range edit {
			f(r)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
	return v
}

// 旧结构（v7 及以前）的配置不迁移：作废并备份，管理密码保留。
func TestOldConfigReset(t *testing.T) {
	dir := t.TempDir()
	const old = `{"auth":{"hash":"h","salt":"s"},"config":{"version":7,"baseConfig":"mode: rule",
		"profiles":[{"id":"1111111111111111","name":"P1","token":"t1","groups":[],"sources":[]}],"templates":[]}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := s.Config()
	if c.Version != configVersion || len(c.Profiles) != 1 || c.Profiles[0].Token == "t1" || !s.HasPassword() {
		t.Errorf("旧配置应作废、密码保留: %+v", c)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "config.json.bak")); err != nil || string(b) != old {
		t.Errorf("没有留下原文件的备份: %v", err)
	}
	s2, err := OpenStore(dir) // 重新打开不应再次重置
	if err != nil || !reflect.DeepEqual(s.Config(), s2.Config()) {
		t.Errorf("重新打开后配置变了: %v", err)
	}
}

func TestValidateSourcesAndRefs(t *testing.T) {
	cfg := defaultConfig()
	cfg.Sources = []Source{
		{ID: "aaaaaaaaaaaaaaaa", Name: "A", URL: "https://a.example/s"},
		{ID: "not-a-valid-id", Name: "B", Kind: "manual", Nodes: "- {name: x, type: http, server: 1.1.1.1, port: 80}", URL: "https://leftover.example"},
		{ID: "aaaaaaaaaaaaaaaa", Name: "和 A 撞 ID"},
	}
	p := &cfg.Profiles[0]
	p.Sources = []SourceRef{{Source: "not-a-valid-id", Prefix: "[B] "}, {Source: "aaaaaaaaaaaaaaaa", Enabled: true}, {Source: "gone"}, {Source: ""}}
	p.Base = "gone"
	cfg.Profiles = append(cfg.Profiles,
		Profile{Name: "底板是手动节点", Base: "not-a-valid-id", Sources: []SourceRef{{Source: "not-a-valid-id"}, {Source: "aaaaaaaaaaaaaaaa"}}},
		Profile{Name: "只有手动节点", Base: "not-a-valid-id", Sources: []SourceRef{{Source: "not-a-valid-id"}}},
		Profile{Name: "底板正常", Base: "aaaaaaaaaaaaaaaa", Sources: []SourceRef{{Source: "aaaaaaaaaaaaaaaa"}}},
	)
	if err := validateConfig(&cfg, map[string]string{p.ID: "tok"}); err != nil {
		t.Fatal(err)
	}
	p = &cfg.Profiles[0]
	b := cfg.Sources[1]
	if !validID(b.ID) || b.URL != "" || cfg.Sources[2].ID == "aaaaaaaaaaaaaaaa" || cfg.Sources[2].Kind != "remote" {
		t.Errorf("来源没有被规范化: %+v", cfg.Sources)
	}
	// 悬空的引用被去掉；ID 被重新生成的来源，引用跟着改
	if want := []SourceRef{{Source: b.ID, Prefix: "[B] "}, {Source: "aaaaaaaaaaaaaaaa", Enabled: true}}; !reflect.DeepEqual(p.Sources, want) || p.Token != "tok" {
		t.Errorf("引用 = %+v", p.Sources)
	}
	// 底板不在本组或不是远程来源时，改用本组第一个远程来源，没有就留空
	var bases []string
	for _, p := range cfg.Profiles {
		bases = append(bases, p.Base)
	}
	if want := []string{"aaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaa", "", "aaaaaaaaaaaaaaaa"}; !reflect.DeepEqual(bases, want) {
		t.Errorf("底板 = %q", bases)
	}

	cfg.Sources[1].Nodes = "这不是节点"
	if err := validateConfig(&cfg, nil); err == nil || !strings.Contains(err.Error(), "B") {
		t.Errorf("手动节点格式错误应当报错: %v", err)
	}
}

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

// 预览用请求里带来的（未保存的）配置生成，不保存；来源的拉取状态在失败后保留上次成功的信息。
func TestPreviewUnsavedAndSourceStatus(t *testing.T) {
	var fail atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Subscription-Userinfo", "upload=1; download=2; total=10; expire=4102444800")
		fmt.Fprint(w, "proxies:\n  - {name: a, type: http, server: 1.1.1.1, port: 80}\n  - {name: b, type: http, server: 1.1.1.2, port: 80}\n"+
			"proxy-groups:\n  - {name: 选择, type: select, proxies: [a, b]}\nrules:\n  - MATCH,选择\n")
	}))
	defer up.Close()

	a, do := newTestApp(t)
	before := a.store.Config()
	draft := a.store.Config()
	const id = "bbbbbbbbbbbbbbbb"
	draft.Sources = []Source{{ID: id, Name: "机场", URL: up.URL}}
	draft.Profiles = append(draft.Profiles, Profile{Name: "还没保存的组", Base: id, Sources: []SourceRef{{Source: id, Enabled: true, Exclude: "b"}}})
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

	status := func() SourceState {
		t.Helper()
		return decode[map[string]SourceState](t, do("GET", "/api/source-status", ""))[id]
	}
	ok := status()
	if ok.Error != "" || ok.Count != 2 || ok.OKTime.IsZero() || !strings.Contains(ok.Userinfo, "total=10") ||
		!reflect.DeepEqual(ok.Groups, []string{"选择"}) {
		t.Errorf("成功后的状态: %+v", ok)
	}
	fail.Store(true)
	if res := preview(true); len(res.Subs) != 1 || res.Subs[0].Error == "" || !res.Subs[0].Stale {
		t.Errorf("上游失败时应回退到旧缓存并带上错误: %+v", res.Subs)
	}
	bad := status()
	if bad.Error == "" || bad.Count != 2 || !bad.OKTime.Equal(ok.OKTime) || bad.Time.Before(ok.OKTime) { // Windows 上时钟精度有限，两次 time.Now() 可能相等
		t.Errorf("失败后应保留上次成功的信息并记下错误: %+v", bad)
	}
	// 重启后从磁盘缓存恢复成功的那部分
	if st := NewFetcher(filepath.Dir(a.fetcher.dir)).States()[id]; st.Count != 2 || st.Error != "" {
		t.Errorf("重启后的状态: %+v", st)
	}
}

// 只用缓存的预览：没有缓存时返回 noCache 且不请求上游；有缓存时（哪怕已过期）直接用缓存生成，同样不请求上游。
func TestPreviewCachedOnly(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "proxies:\n  - {name: a, type: http, server: 1.1.1.1, port: 80}\nproxy-groups:\n  - {name: 选择, type: select, proxies: [a]}\nrules:\n  - MATCH,选择\n")
	}))
	defer up.Close()
	_, do := newTestApp(t)
	draft := Config{}
	const id, other = "cccccccccccccccc", "dddddddddddddddd"
	draft.CacheMinutes = 30
	draft.Sources = []Source{{ID: id, Name: "机场", URL: up.URL}, {ID: other, Name: "另一个", URL: up.URL + "/x"}}
	draft.Profiles = []Profile{{Name: "组", Base: id, Sources: []SourceRef{{Source: id, Enabled: true}, {Source: other, Enabled: true}}}}
	type result struct {
		NoCache bool
		Cached  bool
		Total   int
		Now     time.Time
		Subs    []SubStatus
	}
	preview := func(extra map[string]any) result {
		t.Helper()
		body := map[string]any{"config": draft, "profile": 0}
		maps.Copy(body, extra)
		b, _ := json.Marshal(body)
		return decode[result](t, do("POST", "/api/preview", string(b)))
	}
	if res := preview(map[string]any{"cached": true}); !res.NoCache || hits.Load() != 0 {
		t.Fatalf("没有缓存时应返回 noCache 且不请求上游: %+v，请求 %d 次", res, hits.Load())
	}
	// 只拉一次底板：另一个来源暂时不引用
	draft.Profiles[0].Sources[1].Enabled = false
	if res := preview(nil); res.Total != 1 || res.Cached {
		t.Fatalf("正常预览: %+v", res)
	}
	draft.Profiles[0].Sources[1].Enabled = true
	n := hits.Load()
	res := preview(map[string]any{"cached": true})
	if hits.Load() != n || !res.Cached || res.NoCache || res.Total != 1 || res.Now.IsZero() || len(res.Subs) != 2 {
		t.Fatalf("有缓存时应直接用缓存生成: %+v，请求 %d 次", res, hits.Load()-n)
	}
	if s := res.Subs[0]; s.FetchedAt.IsZero() || !s.ExpiresAt.Equal(s.FetchedAt.Add(30*time.Minute)) || s.NoCache {
		t.Errorf("底板的缓存时间: %+v", s)
	}
	if s := res.Subs[1]; !s.NoCache || s.Error != "" || !s.FetchedAt.IsZero() {
		t.Errorf("没有缓存的来源应标记 noCache: %+v", s)
	}
	// 缓存时长改成 0：缓存已过期，但只用缓存时照用
	draft.CacheMinutes = 0
	if res := preview(map[string]any{"cached": true}); hits.Load() != n || res.Total != 1 {
		t.Errorf("过期的缓存也应直接使用: %+v，请求 %d 次", res, hits.Load()-n)
	}
}

func TestFormatParserAPI(t *testing.T) {
	_, do := newTestApp(t)
	res := decode[map[string]string](t, do("POST", "/api/format-parser", `{"text":"yaml:\n    prepend-rules:\n    - MATCH,DIRECT\n"}`))
	if res["text"] != "yaml:\n  prepend-rules:\n    - MATCH,DIRECT\n" {
		t.Errorf("格式化结果 = %q", res["text"])
	}
	if w := do("POST", "/api/format-parser", `{"text":"a: [1\n"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "YAML") {
		t.Errorf("YAML 有错时应返回 400: %d %s", w.Code, w.Body)
	}
}
