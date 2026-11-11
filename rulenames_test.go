package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// 规则清单：按目录结构筛选、去扩展名、排序；失败时不动已有的清单；清单按来源 ID 保存并在重启后还在
func TestRuleNames(t *testing.T) {
	var auth []string
	limited := false
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		if limited {
			w.Header().Set("X-RateLimit-Remaining", "0")
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/repos/o/r/git/trees/master:rule/Clash":
			fmt.Fprint(w, `{"tree":[{"path":"README.md","type":"blob"},{"path":"OpenAI","type":"tree"},{"path":"115","type":"tree"},{"path":"apple","type":"tree"}]}`)
		case "/repos/o/r/git/trees/release":
			fmt.Fprint(w, `{"tree":[{"path":"proxy.txt","type":"blob"},{"path":"cncidr.txt","type":"blob"},{"path":"x.yaml","type":"blob"},{"path":".txt","type":"blob"},{"path":"sub","type":"tree"}]}`)
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	defer gh.Close()
	old := githubAPI
	githubAPI = gh.URL
	defer func() { githubAPI = old }()
	t.Setenv("GITHUB_TOKEN", "")

	a, do := newTestApp(t)
	cfg := a.store.Config()
	if len(cfg.RuleSources) == 0 {
		t.Fatal("新配置没有内置的规则来源")
	}
	id := cfg.RuleSources[0].ID
	pull := func(body string) *httptest.ResponseRecorder { return do("POST", "/api/rule-names", body) }

	got := decode[RuleNames](t, pull(`{"id":"`+id+`","repo":"o/r","ref":"master","dir":"/rule/Clash/","layout":"dir"}`))
	if !reflect.DeepEqual(got.Names, []string{"115", "apple", "OpenAI"}) || got.Updated.IsZero() {
		t.Errorf("子目录形式的清单不符: %+v", got)
	}
	// 文件形式：只取指定扩展名（没写点也行），规则名去掉扩展名。来源还没保存（ID 不在配置里）也能拉
	const unsaved = "0123456789abcdef"
	got = decode[RuleNames](t, pull(`{"id":"`+unsaved+`","repo":"o/r","ref":"release","layout":"file","ext":"txt"}`))
	if !reflect.DeepEqual(got.Names, []string{"cncidr", "proxy"}) {
		t.Errorf("文件形式的清单不符: %+v", got)
	}
	if auth[len(auth)-1] != "" {
		t.Errorf("没设置 GITHUB_TOKEN 时不应带 Authorization: %q", auth[len(auth)-1])
	}

	// 各种失败：都不改动已有的清单
	for body, want := range map[string]string{
		`{"id":"` + id + `","repo":"o/r","ref":"nope","layout":"dir"}`:                  "找不到",
		`{"id":"` + id + `","repo":"o/r","ref":"release","layout":"dir","dir":"sub"}`:   "找不到",
		`{"id":"` + id + `","repo":"o/r","ref":"release","layout":"file","ext":".mrs"}`: "没有扩展名为 .mrs",
		`{"id":"` + id + `","repo":"https://github.com/o/r","ref":"master"}`:            "owner/repo",
		`{"id":"` + id + `","repo":"o/r"}`:                                              "分支",
		`{"id":"` + id + `","ref":"master"}`:                                            "没有填写 GitHub 仓库",
		`{"id":"bad","repo":"o/r","ref":"master"}`:                                      "ID 无效",
	} {
		if w := pull(body); w.Code == http.StatusOK || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: 期望失败并提示「%s」，得到 %d %s", body, want, w.Code, w.Body)
		}
	}
	limited = true
	t.Setenv("GITHUB_TOKEN", "tok")
	if w := pull(`{"id":"` + id + `","repo":"o/r","ref":"master","dir":"rule/Clash","layout":"dir"}`); w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "GITHUB_TOKEN") {
		t.Errorf("限流时的提示不符: %d %s", w.Code, w.Body)
	}
	if auth[len(auth)-1] != "Bearer tok" {
		t.Errorf("设置了 GITHUB_TOKEN 应带上令牌: %q", auth[len(auth)-1])
	}

	all := decode[map[string]RuleNames](t, do("GET", "/api/rule-names", ""))
	if len(all) != 2 || len(all[id].Names) != 3 || len(all[unsaved].Names) != 2 {
		t.Errorf("保存的清单不符: %+v", all)
	}
	// 重新打开还在；之后再拉取时，配置里已经没有的来源（unsaved 从未保存）的清单被清掉
	dir := filepath.Dir(a.ruleNames.path)
	if re := OpenRuleNames(dir).All(); !reflect.DeepEqual(re[id].Names, all[id].Names) || len(re) != 2 {
		t.Errorf("重新打开后清单不符: %+v", re)
	}
	limited = false
	decode[RuleNames](t, pull(`{"id":"`+id+`","repo":"o/r","ref":"master","dir":"rule/Clash","layout":"dir"}`))
	if all = a.ruleNames.All(); len(all) != 1 || len(all[id].Names) != 3 {
		t.Errorf("已删除来源的清单没有清掉: %+v", all)
	}
	// 文件损坏时从空的开始
	if err := os.WriteFile(filepath.Join(dir, "rulenames.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if re := OpenRuleNames(dir).All(); len(re) != 0 {
		t.Errorf("损坏的清单文件应被忽略: %+v", re)
	}
}

// 保存配置时规则来源的校验和规范化
func TestValidateRuleSources(t *testing.T) {
	a, do := newTestApp(t)
	cfg := a.store.Config()
	cfg.RuleSources = []RuleSource{
		{ID: "x", Name: " ", Repo: " o/r ", Ref: "main", Dir: "/a/b/", Layout: "weird", Ext: "list", URL: "https://x.example/{name}.list", Behavior: "?", Format: "?", Pinned: []string{" A ", "", "A", "B"}},
		{ID: "aaaaaaaaaaaaaaaa", Name: "只有置顶", Layout: "dir", Ext: ".x", Behavior: "ipcidr", Format: "mrs"},
		{ID: "aaaaaaaaaaaaaaaa", Name: "重复的 ID"},
	}
	b, _ := json.Marshal(cfg)
	got := decode[Config](t, do("PUT", "/api/config", string(b))).RuleSources
	s := got[0]
	if !validID(s.ID) || s.Name != "o/r" || s.Repo != "o/r" || s.Dir != "a/b" || s.Layout != "file" || s.Ext != ".list" || s.Behavior != "classical" || s.Format != "yaml" || !reflect.DeepEqual(s.Pinned, []string{"A", "B"}) {
		t.Errorf("规范化结果不符: %+v", s)
	}
	if s = got[1]; s.ID != "aaaaaaaaaaaaaaaa" || s.Ext != "" || s.Behavior != "ipcidr" || s.Format != "mrs" || s.Pinned == nil {
		t.Errorf("规范化结果不符: %+v", s)
	}
	if got[2].ID == got[1].ID || !validID(got[2].ID) {
		t.Errorf("重复的 ID 没有重新生成: %+v", got[2])
	}
	for _, bad := range []RuleSource{
		{Name: "a", Repo: "norepo", Ref: "main"},
		{Name: "a", Repo: "o/r"},
		{Name: "a", Repo: "o/r", Ref: "a:b"},
		{Name: "a", URL: "ftp://x/{name}"},
	} {
		cfg.RuleSources = []RuleSource{bad}
		b, _ = json.Marshal(cfg)
		if w := do("PUT", "/api/config", string(b)); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "规则来源") {
			t.Errorf("%+v 应被拒绝: %d %s", bad, w.Code, w.Body)
		}
	}
}
