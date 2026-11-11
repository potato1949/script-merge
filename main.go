package main

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

//go:embed web/index.html
var webFS embed.FS

const (
	sessionCookie = "sm_session"
	sessionTTL    = 7 * 24 * time.Hour
)

type App struct {
	store       *Store
	fetcher     *Fetcher
	envPassword string // 来自 ADMIN_PASSWORD；非空时网页端不可修改密码

	mu       sync.Mutex
	sessions map[string]time.Time
	loginMu  sync.Mutex // 串行化登录尝试以限制爆破速度
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	dataDir := env("DATA_DIR", "./data")
	store, err := OpenStore(dataDir)
	if err != nil {
		log.Fatalf("打开数据目录失败: %v", err)
	}
	app := &App{
		store:       store,
		fetcher:     NewFetcher(dataDir),
		envPassword: os.Getenv("ADMIN_PASSWORD"),
		sessions:    map[string]time.Time{},
	}
	addr := ":" + env("PORT", "8080")
	log.Printf("script-merge 已启动，监听 %s，数据目录 %s", addr, dataDir)
	srv := &http.Server{Addr: addr, Handler: app.routes(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := webFS.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("GET /api/status", a.handleStatus)
	mux.HandleFunc("POST /api/setup", a.handleSetup)
	mux.HandleFunc("POST /api/login", a.handleLogin)
	mux.HandleFunc("POST /api/logout", a.handleLogout)
	mux.HandleFunc("GET /api/config", a.auth(a.handleGetConfig))
	mux.HandleFunc("PUT /api/config", a.auth(a.handlePutConfig))
	mux.HandleFunc("POST /api/password", a.auth(a.handlePassword))
	mux.HandleFunc("POST /api/token", a.auth(a.handleResetToken))
	mux.HandleFunc("GET /api/preview", a.auth(a.handlePreview))
	mux.HandleFunc("GET /sub/{token}", a.handleSub)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return false
	}
	return true
}

// ---------- 认证 ----------

func (a *App) initialized() bool { return a.envPassword != "" || a.store.HasPassword() }

func (a *App) checkPassword(pw string) bool {
	if a.envPassword != "" {
		return subtle.ConstantTimeCompare([]byte(pw), []byte(a.envPassword)) == 1
	}
	return a.store.CheckPassword(pw)
}

func (a *App) authed(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[c.Value]
	if !ok || time.Now().After(exp) {
		delete(a.sessions, c.Value)
		return false
	}
	return true
}

func (a *App) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authed(r) {
			writeErr(w, http.StatusUnauthorized, "未登录")
			return
		}
		next(w, r)
	}
}

func (a *App) startSession(w http.ResponseWriter, r *http.Request) {
	id := randHex(32)
	a.mu.Lock()
	now := time.Now()
	for k, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, k)
		}
	}
	a.sessions[id] = now.Add(sessionTTL)
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: id, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds()),
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{
		"initialized": a.initialized(),
		"authed":      a.authed(r),
		"envPassword": a.envPassword != "",
	})
}

func validPassword(w http.ResponseWriter, pw string) bool {
	if len(pw) < 6 {
		writeErr(w, http.StatusBadRequest, "密码至少 6 位")
		return false
	}
	return true
}

// handleSetup 仅在尚未设置密码时可用，用于首次访问设置管理密码。
func (a *App) handleSetup(w http.ResponseWriter, r *http.Request) {
	var req struct{ Password string }
	if !readJSON(w, r, &req) || !validPassword(w, req.Password) {
		return
	}
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	if a.initialized() {
		writeErr(w, http.StatusForbidden, "管理密码已设置")
		return
	}
	if err := a.store.SetPassword(req.Password); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.startSession(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct{ Password string }
	if !readJSON(w, r, &req) {
		return
	}
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	if !a.checkPassword(req.Password) {
		time.Sleep(time.Second)
		writeErr(w, http.StatusUnauthorized, "密码错误")
		return
	}
	a.startSession(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) handlePassword(w http.ResponseWriter, r *http.Request) {
	var req struct{ Old, New string }
	if !readJSON(w, r, &req) {
		return
	}
	if a.envPassword != "" {
		writeErr(w, http.StatusBadRequest, "密码由环境变量 ADMIN_PASSWORD 管理，无法在网页修改")
		return
	}
	if !a.store.CheckPassword(req.Old) {
		writeErr(w, http.StatusBadRequest, "原密码错误")
		return
	}
	if !validPassword(w, req.New) {
		return
	}
	if err := a.store.SetPassword(req.New); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------- 配置 ----------

func (a *App) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.store.Config())
}

// validateConfig 校验并规范化前端提交的配置。tokens 是现有订阅组 ID → 令牌。
func validateConfig(c *Config, tokens map[string]string) error {
	var tmp any
	if err := yaml.Unmarshal([]byte(c.BaseConfig), &tmp); err != nil {
		return errors.New("基础配置不是合法的 YAML: " + err.Error())
	}
	if _, ok := tmp.(map[string]any); !ok && strings.TrimSpace(c.BaseConfig) != "" {
		return errors.New("基础配置必须是 YAML 对象")
	}
	c.Version = configVersion
	c.LegacyToken, c.LegacyFileName, c.LegacySubscriptions, c.LegacyCustomProxies = "", "", nil, ""
	c.LegacyGroups, c.LegacyRulesBefore, c.LegacyRuleSets, c.LegacyRulesAfter, c.LegacyParser = nil, "", nil, "", ""
	if len(c.Profiles) == 0 {
		return errors.New("至少需要保留一个订阅组")
	}
	for _, ua := range []*string{&c.UserAgent, &c.UnlockUserAgent} {
		*ua = strings.TrimSpace(*ua)
		if len(*ua) > 256 || strings.ContainsFunc(*ua, unicode.IsControl) {
			return errors.New("全局设置里的 UA 不能超过 256 个字符，也不能包含换行等控制字符")
		}
	}
	// ID 只接受服务端生成的格式（前端新建来源、模板时也按这个格式生成，以便保存前就能互相引用）：
	// 来源 ID 用作缓存文件名，订阅组 ID 用于保留令牌。格式不对或重复的一律重新生成
	ids := map[string]bool{}
	fixID := func(id *string) {
		if !validID(*id) || ids[*id] {
			*id = randHex(8)
		}
		ids[*id] = true
	}
	for pi := range c.Profiles {
		p := &c.Profiles[pi]
		fixID(&p.ID)
		p.Token = firstNonEmpty(tokens[p.ID], randHex(16)) // 令牌只能通过重置接口修改
		if strings.TrimSpace(p.Name) == "" {
			p.Name = "订阅组 " + strconv.Itoa(pi+1)
		}
		if err := yaml.Unmarshal([]byte(p.Parser), &tmp); err != nil {
			return errors.New("订阅组「" + p.Name + "」的 Parser 不是合法的 YAML: " + err.Error())
		}
		p.LegacyCustomProxies, p.LegacySubscriptions = "", nil
		if p.Groups == nil {
			p.Groups = []Group{}
		}
		if p.RuleSets == nil {
			p.RuleSets = []RuleSet{}
		}
	}
	if c.Sources == nil {
		c.Sources = []Source{}
	}
	sourceIDs := map[string]string{} // 提交时的来源 ID → 规范化后的 ID，用来修正订阅组里的引用
	for i := range c.Sources {
		s := &c.Sources[i]
		old := s.ID
		fixID(&s.ID)
		if _, dup := sourceIDs[old]; old != "" && !dup {
			sourceIDs[old] = s.ID
		}
		if strings.TrimSpace(s.Name) == "" {
			s.Name = "来源 " + strconv.Itoa(i+1)
		}
		if s.Kind == "manual" {
			s.URL, s.UnlockURL, s.UAMode = "", "", ""
			if _, err := parseCustomProxies(s.Nodes); err != nil {
				return errors.New("节点来源「" + s.Name + "」: " + err.Error())
			}
			continue
		}
		s.Kind, s.Nodes = "remote", ""
		for _, u := range []string{s.URL, s.UnlockURL} {
			if u = strings.TrimSpace(u); u == "" {
				continue
			}
			if pu, err := url.Parse(u); err != nil || (pu.Scheme != "http" && pu.Scheme != "https") {
				return errors.New("节点来源「" + s.Name + "」的地址必须以 http:// 或 https:// 开头")
			}
		}
		if s.UAMode != "passthrough" {
			s.UAMode = "fixed"
		}
	}
	for pi := range c.Profiles {
		p := &c.Profiles[pi]
		refs := []SourceRef{}
		for _, r := range p.Sources {
			if id, ok := sourceIDs[r.Source]; ok { // 引用的来源不存在（已被删除）就去掉这条引用
				r.Source = id
				refs = append(refs, r)
			}
		}
		p.Sources = refs
	}
	if c.Templates == nil {
		c.Templates = []Template{}
	}
	for i := range c.Templates {
		t := &c.Templates[i]
		fixID(&t.ID)
		switch t.Kind {
		case "groups":
			t.Rules, t.Parser = Rules{}, ""
		case "rules":
			t.Groups, t.Parser = nil, ""
		case "parser":
			t.Groups, t.Rules = nil, Rules{}
			if err := yaml.Unmarshal([]byte(t.Parser), &tmp); err != nil {
				return errors.New("模板「" + t.Name + "」的 Parser 不是合法的 YAML: " + err.Error())
			}
		default:
			return errors.New("模板「" + t.Name + "」的类型无效")
		}
		if strings.TrimSpace(t.Name) == "" {
			t.Name = "未命名模板"
		}
	}
	if c.CacheMinutes < 0 {
		c.CacheMinutes = 0
	}
	if c.UpdateInterval < 1 {
		c.UpdateInterval = 12
	}
	return nil
}

func (a *App) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var c Config
	if !readJSON(w, r, &c) {
		return
	}
	tokens := map[string]string{}
	for _, p := range a.store.Config().Profiles {
		tokens[p.ID] = p.Token
	}
	if err := validateConfig(&c, tokens); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.store.SetConfig(c); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (a *App) handleResetToken(w http.ResponseWriter, r *http.Request) {
	var req struct{ Profile string }
	if !readJSON(w, r, &req) {
		return
	}
	c := a.store.Config()
	p := findProfile(c, func(p Profile) bool { return p.ID == req.Profile })
	if p == nil {
		writeErr(w, http.StatusNotFound, "订阅组不存在")
		return
	}
	p.Token = randHex(16)
	if err := a.store.SetConfig(c); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// ---------- 生成 ----------

func findProfile(c Config, match func(Profile) bool) *Profile {
	for i := range c.Profiles {
		if match(c.Profiles[i]) {
			return &c.Profiles[i]
		}
	}
	return nil
}

// clientUA 取请求的 UA 作为透传的依据，限制长度以免被用来撑大缓存键。
func clientUA(ua string) string {
	if ua = strings.TrimSpace(ua); len(ua) > 256 {
		ua = ua[:256]
	}
	return ua
}

// handlePreview 以 ?ua= 指定的 UA 模拟一次客户端拉取。
func (a *App) handlePreview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cfg := a.store.Config()
	p := findProfile(cfg, func(p Profile) bool { return p.ID == q.Get("profile") })
	if p == nil {
		writeErr(w, http.StatusNotFound, "订阅组不存在")
		return
	}
	res, err := Build(cfg, *p, a.fetcher, clientUA(q.Get("ua")), q.Get("refresh") == "1")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"yaml": string(res.YAML), "subs": res.Subs, "warnings": res.Warnings,
		"total": res.Total, "userinfo": res.Userinfo,
	})
}

func (a *App) handleSub(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Config()
	token := []byte(r.PathValue("token"))
	p := findProfile(cfg, func(p Profile) bool { return subtle.ConstantTimeCompare(token, []byte(p.Token)) == 1 })
	if p == nil {
		http.NotFound(w, r)
		return
	}
	res, err := Build(cfg, *p, a.fetcher, clientUA(r.UserAgent()), r.URL.Query().Get("refresh") == "1")
	if err != nil {
		log.Printf("生成订阅失败: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, s := range res.Subs {
		if s.Error != "" {
			log.Printf("订阅「%s」拉取失败（使用旧缓存: %v）: %s", s.Name, s.Stale, s.Error)
		}
	}
	if res.Total == 0 {
		// 不返回空配置，避免客户端用它覆盖掉可用的旧配置
		http.Error(w, "没有可用节点：所有订阅均拉取失败或未配置订阅", http.StatusBadGateway)
		return
	}
	name := firstNonEmpty(strings.TrimSpace(p.FileName), strings.TrimSpace(p.Name), "merged")
	h := w.Header()
	h.Set("Content-Type", "text/yaml; charset=utf-8")
	h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	h.Set("Profile-Update-Interval", strconv.Itoa(cfg.UpdateInterval))
	if res.Userinfo != "" {
		h.Set("Subscription-Userinfo", res.Userinfo)
	}
	_, _ = w.Write(res.YAML)
}
