package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// githubAPI 是列目录用的 GitHub 接口地址，测试里会换成 httptest 的地址。
var githubAPI = "https://api.github.com"

// maxRuleNames 限制一个来源的规则清单条数，防止填错目录时把一个巨大的列表存下来。
const maxRuleNames = 20000

// RuleNames 是一个规则来源最近一次拉到的规则清单。
type RuleNames struct {
	Names   []string  `json:"names"`
	Updated time.Time `json:"updated"`
}

// RuleNameStore 保存各规则来源的规则清单（来源 ID → 清单），存在 DATA_DIR/rulenames.json。
// 清单只在用户手动拉取时更新，不属于配置，不随导出。
type RuleNameStore struct {
	mu   sync.Mutex
	path string
	data map[string]RuleNames
}

func OpenRuleNames(dataDir string) *RuleNameStore {
	s := &RuleNameStore{path: filepath.Join(dataDir, "rulenames.json"), data: map[string]RuleNames{}}
	if b, err := os.ReadFile(s.path); err == nil {
		if json.Unmarshal(b, &s.data) != nil || s.data == nil { // 损坏就从空的开始
			s.data = map[string]RuleNames{}
		}
	}
	return s
}

func (s *RuleNameStore) All() map[string]RuleNames {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]RuleNames, len(s.data))
	for id, l := range s.data {
		out[id] = l
	}
	return out
}

// Set 存下一个来源的清单，同时丢掉 keep 之外的来源留下的清单（来源已被删除）。
func (s *RuleNameStore) Set(id string, l RuleNames, keep map[string]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for old := range s.data {
		if !keep[old] {
			delete(s.data, old)
		}
	}
	s.data[id] = l
	b, err := json.Marshal(s.data)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// normalize 校验并规范化一个规则来源，保存配置和拉取清单前都会调用。
func (s *RuleSource) normalize() error {
	for _, f := range []*string{&s.Name, &s.Repo, &s.Ref, &s.Dir, &s.Ext, &s.URL, &s.IPCIDR, &s.Prefix} {
		*f = strings.TrimSpace(*f)
		if len(*f) > 500 || strings.ContainsFunc(*f, unicode.IsControl) {
			return errors.New("各项不能超过 500 个字符，也不能包含换行等控制字符")
		}
	}
	s.Dir = strings.Trim(s.Dir, "/")
	if s.Layout != "dir" {
		s.Layout = "file"
	}
	if s.Layout == "dir" {
		s.Ext = ""
	} else if s.Ext != "" && !strings.HasPrefix(s.Ext, ".") {
		s.Ext = "." + s.Ext
	}
	if s.Repo != "" {
		owner, name, ok := strings.Cut(s.Repo, "/")
		if !ok || !repoPart(owner) || !repoPart(name) {
			return errors.New("GitHub 仓库要写成 owner/repo 的形式")
		}
		if s.Ref == "" {
			return errors.New("填了仓库就要填分支")
		}
		if strings.ContainsAny(s.Ref+s.Dir, " :?#\\") {
			return errors.New("分支和目录不能包含空格和 : ? # \\")
		}
	}
	if s.URL != "" {
		if u, err := url.Parse(s.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return errors.New("规则文件地址必须以 http:// 或 https:// 开头")
		}
	}
	if !map[string]bool{"classical": true, "domain": true, "ipcidr": true}[s.Behavior] {
		s.Behavior = "classical"
	}
	if !map[string]bool{"yaml": true, "text": true, "mrs": true}[s.Format] {
		s.Format = "yaml"
	}
	pinned, seen := []string{}, map[string]bool{}
	for _, n := range s.Pinned {
		if n = strings.TrimSpace(n); n != "" && !seen[n] && len(n) <= 200 {
			seen[n] = true
			pinned = append(pinned, n)
		}
	}
	s.Pinned = pinned
	return nil
}

func repoPart(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	return strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") == ""
}

// fetchRuleNames 用 GitHub 的目录接口列出规则来源里的全部规则名。
// 一次请求列一层目录（git/trees/{分支}:{目录}），不递归；和上游订阅一样走环境变量里的代理。
// 匿名访问按出口 IP 每小时限 60 次，设置环境变量 GITHUB_TOKEN 后改用令牌访问。
func fetchRuleNames(src RuleSource) ([]string, error) {
	if src.Repo == "" {
		return nil, errors.New("没有填写 GitHub 仓库，无法拉取规则清单")
	}
	tree := url.PathEscape(src.Ref)
	if src.Dir != "" {
		parts := strings.Split(src.Dir, "/")
		for i := range parts {
			parts[i] = url.PathEscape(parts[i])
		}
		tree += ":" + strings.Join(parts, "/")
	}
	req, err := http.NewRequest(http.MethodGet, githubAPI+"/repos/"+src.Repo+"/git/trees/"+tree, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "script-merge")
	req.Header.Set("Accept", "application/vnd.github+json")
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 40 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("访问 GitHub 失败: " + err.Error())
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, errors.New("读取 GitHub 的响应失败: " + err.Error())
	}
	var out struct {
		Message string `json:"message"`
		Tree    []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	parseErr := json.Unmarshal(body, &out)
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errors.New("GitHub 上找不到这个仓库、分支或目录")
	case (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) && resp.Header.Get("X-RateLimit-Remaining") == "0":
		return nil, errors.New("GitHub 接口的访问次数用完了（匿名访问按出口 IP 每小时 60 次），过一会儿再试，或给服务设置环境变量 GITHUB_TOKEN")
	case resp.StatusCode != http.StatusOK:
		return nil, errors.New("GitHub 返回 " + strconv.Itoa(resp.StatusCode) + " " + out.Message)
	case parseErr != nil:
		return nil, errors.New("GitHub 的响应无法解析: " + parseErr.Error())
	}
	names := []string{}
	for _, e := range out.Tree {
		if src.Layout == "dir" {
			if e.Type == "tree" {
				names = append(names, e.Path)
			}
		} else if e.Type == "blob" && strings.HasSuffix(e.Path, src.Ext) && len(e.Path) > len(src.Ext) {
			names = append(names, strings.TrimSuffix(e.Path, src.Ext))
		}
	}
	if len(names) == 0 {
		if src.Layout == "dir" {
			return nil, errors.New("这个目录下没有子目录，检查目录和「目录结构」是否填对")
		}
		return nil, errors.New("这个目录下没有扩展名为 " + firstNonEmpty(src.Ext, "（任意）") + " 的文件，检查目录和扩展名是否填对")
	}
	if len(names) > maxRuleNames {
		return nil, errors.New("这个目录下有 " + strconv.Itoa(len(names)) + " 条，超过了 " + strconv.Itoa(maxRuleNames) + " 条的上限")
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := strings.ToLower(names[i]), strings.ToLower(names[j])
		if a != b {
			return a < b
		}
		return names[i] < names[j]
	})
	return names, nil
}

func (a *App) handleGetRuleNames(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.ruleNames.All())
}

// handlePullRuleNames 拉取一个规则来源的规则清单并存下来。
// 请求体就是来源本身（而不是 ID），这样在「规则来源」页改了仓库、目录后不用先保存就能拉取。
func (a *App) handlePullRuleNames(w http.ResponseWriter, r *http.Request) {
	var src RuleSource
	if !readJSON(w, r, &src) {
		return
	}
	if !validID(src.ID) {
		writeErr(w, http.StatusBadRequest, "规则来源的 ID 无效")
		return
	}
	if err := src.normalize(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	names, err := fetchRuleNames(src)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	keep := map[string]bool{}
	for _, s := range a.store.Config().RuleSources {
		keep[s.ID] = true
	}
	list := RuleNames{Names: names, Updated: time.Now()}
	if err := a.ruleNames.Set(src.ID, list, keep); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}
