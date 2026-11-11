package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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
	a = &App{store: store, fetcher: NewFetcher(dir), access: OpenAccessLog(dir, 0), ruleNames: OpenRuleNames(dir), sessions: map[string]time.Time{}}
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

func TestExportImport(t *testing.T) {
	a, do := newTestApp(t)
	cfg := a.store.Config()
	cfg.CacheMinutes = 42
	cfg.Sources = []Source{
		{ID: "aaaaaaaaaaaaaaaa", Name: "机场A", URL: "https://a.example/SECRET-LINK", UnlockURL: "https://a.example/SECRET-UNLOCK", UAMode: "passthrough"},
		{ID: "bbbbbbbbbbbbbbbb", Name: "自建", Kind: "manual", Nodes: "- {name: x, type: http, server: 1.1.1.1, port: 80, password: SECRET-NODE}"},
		{ID: "cccccccccccccccc", Name: "没被这个组用到", URL: "https://c.example/s"},
	}
	cfg.Profiles[0].Sources = []SourceRef{{Source: "aaaaaaaaaaaaaaaa", Enabled: true, Prefix: "[A] "}, {Source: "bbbbbbbbbbbbbbbb", Enabled: true}}
	cfg.Profiles[0].Applied = map[string]TemplateRef{"groups": {ID: cfg.Templates[0].ID, Hash: "abc"}}
	body, _ := json.Marshal(cfg)
	saved := decode[Config](t, do("PUT", "/api/config", string(body)))
	orig := saved.Profiles[0]

	// 整份导出：带订阅链接和令牌
	w := do("GET", "/api/export", "")
	full := w.Body.String()
	f := decode[exportFile](t, w)
	if f.Kind != "config" || f.Version != configVersion || f.Config.Profiles[0].Token != orig.Token || f.Config.Sources[0].URL != "https://a.example/SECRET-LINK" ||
		!strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Errorf("整份导出不符: %+v", f)
	}

	// 单个订阅组导出：不带链接、解锁地址、手填节点、令牌，只带它引用的来源
	w = do("GET", "/api/export?profile="+orig.ID, "")
	one := w.Body.String()
	pf := decode[exportFile](t, w)
	if strings.Contains(one, "SECRET") || strings.Contains(one, orig.Token) || pf.Kind != "profile" || len(pf.Sources) != 2 || pf.Profile.Applied != nil {
		t.Errorf("订阅组导出不符: %s", one)
	}
	if w := do("GET", "/api/export?profile=nope", ""); w.Code != http.StatusNotFound {
		t.Errorf("不存在的订阅组: HTTP %d", w.Code)
	}

	// 导入单个订阅组：追加，新 ID、新令牌、新来源；原有内容不变
	after := decode[Config](t, do("POST", "/api/import", one))
	if len(after.Profiles) != 2 || len(after.Sources) != 5 || !reflect.DeepEqual(after.Profiles[0], orig) {
		t.Fatalf("导入订阅组后结构不符: %+v", after)
	}
	np := after.Profiles[1]
	if np.ID == orig.ID || np.Token == orig.Token || np.Token == "" || np.Name != orig.Name+"（导入）" || len(np.Groups) != len(orig.Groups) ||
		len(np.Sources) != 2 || np.Sources[0].Source != after.Sources[3].ID || np.Sources[0].Prefix != "[A] " ||
		after.Sources[3].Name != "机场A" || after.Sources[3].URL != "" || after.Sources[3].UAMode != "passthrough" || after.Sources[3].ID == "aaaaaaaaaaaaaaaa" {
		t.Errorf("导入的订阅组不符: %+v / %+v", np, after.Sources)
	}

	// 整份导入到另一个实例：替换全部配置，令牌保留
	_, do2 := newTestApp(t)
	got := decode[Config](t, do2("POST", "/api/import", full))
	if !reflect.DeepEqual(got, saved) {
		t.Errorf("整份导入后与导出时不一致:\n%+v\n%+v", got, saved)
	}
	if got2 := decode[Config](t, do2("GET", "/api/config", "")); !reflect.DeepEqual(got2, saved) {
		t.Errorf("导入结果没有保存")
	}

	// 直接上传旧版本的 config.json：先迁移再导入
	const v4 = `{"auth":{"hash":"h","salt":"s"},"config":{"version":4,"baseConfig":"mode: rule","profiles":[
		{"id":"1111111111111111","name":"旧","token":"old-token_1","subscriptions":[{"id":"dddddddddddddddd","name":"S","url":"https://s.example/x","enabled":true,"prefix":"[S] "}]}]}}`
	got = decode[Config](t, do2("POST", "/api/import", v4))
	if got.Version != configVersion || len(got.Profiles) != 1 || got.Profiles[0].Token != "old-token_1" || len(got.Sources) != 1 ||
		got.Sources[0].URL != "https://s.example/x" || got.Profiles[0].Sources[0] != (SourceRef{Source: "dddddddddddddddd", Enabled: true, Prefix: "[S] "}) {
		t.Errorf("旧配置导入不符: %+v", got)
	}

	// 无法导入的文件不改动现有配置
	for _, bad := range []string{`not json`, `{}`, `{"hello":1}`, `{"config":{"version":99,"profiles":[{"name":"x"}]}}`,
		`{"kind":"profile","version":99,"profile":{"name":"x"}}`, `{"config":{"version":5,"profiles":[]}}`,
		`{"config":{"version":5,"baseConfig":"- a\n- b","profiles":[{"name":"x"}]}}`} {
		if w := do2("POST", "/api/import", bad); w.Code != http.StatusBadRequest {
			t.Errorf("%s: HTTP %d", bad, w.Code)
		}
	}
	if got2 := decode[Config](t, do2("GET", "/api/config", "")); !reflect.DeepEqual(got2, got) {
		t.Errorf("导入失败后配置被改动")
	}
}
