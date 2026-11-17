package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// defaultUA 是全局设置里没填固定 UA 时，拉取上游订阅用的 UA
	defaultUA = "clash-verge/v2.4.2"
	// chromeUA 是全局设置里没填解锁 UA 时，访问解锁地址用的 UA
	chromeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

	maxEntriesPerSub = 16               // 每个订阅最多保留多少份（不同链接 / UA 的）缓存
	failBackoff      = 30 * time.Second // 拉取失败后，同一链接 + UA 在这段时间内不再重试
)

// cacheFormat 是缓存内容的格式版本。旧格式的缓存（缺少后来加的字段）视为已过期，但拉取失败时仍可作为旧缓存使用。
//
//	2：加入上游的 hosts、fake-ip-filter、策略组、规则和 rule-providers
//	3：改为保存上游 Clash 配置里节点以外的全部内容（底板机场要用到完整配置）
const cacheFormat = 3

// cacheEntry 是某个来源以某个「链接 + UA」拉取到的结果。
type cacheEntry struct {
	Format    int       `json:"format"`
	Key       string    `json:"key"`
	SubID     string    `json:"subId"`
	Userinfo  string    `json:"userinfo"`
	FetchedAt time.Time `json:"fetchedAt"`
	upstream
}

// upstream 是解析后的上游内容。分享链接列表只有节点；Clash YAML 还有 Doc：
// 去掉 proxies 之后的整份配置（策略组、规则、rule-providers、DNS、hosts 等），作为底板时原样输出。
type upstream struct {
	Proxies []map[string]any `json:"proxies"`
	Doc     map[string]any   `json:"doc,omitempty"`
}

// groups 是上游配置里的策略组（只要带名字的）。
func (u *upstream) groups() []map[string]any { return mapsOf(u.Doc["proxy-groups"], true) }

// hosts 和 fakeIPFilter 是上游配置里的 hosts 与 dns.fake-ip-filter，节点地址经 hosts 映射时要用（见 nodeHosts）。
func (u *upstream) hosts() map[string]any {
	h, _ := u.Doc["hosts"].(map[string]any)
	return h
}

func (u *upstream) fakeIPFilter() []string {
	dns, _ := u.Doc["dns"].(map[string]any)
	return stringsOf(dns["fake-ip-filter"])
}

// FetchMode 决定构建时怎么用缓存。
type FetchMode int

const (
	FetchAuto      FetchMode = iota // 缓存未过期就用缓存，否则拉取上游（客户端拉取订阅的流程）
	FetchRefresh                    // 跳过缓存，直接拉取上游
	FetchCacheOnly                  // 只用已有的缓存（过期的也用），绝不请求上游：打开预览时直接展示
)

// SubStatus 描述一次构建中某个来源的状态。
type SubStatus struct {
	Index     int       `json:"index"` // 在订阅组的来源列表里的下标
	ID        string    `json:"id"`    // 来源 ID
	Name      string    `json:"name"`
	Base      bool      `json:"base"`  // 是不是底板机场
	URL       string    `json:"url"`   // 本次实际使用的上游链接
	UA        string    `json:"ua"`    // 本次实际使用的 UA
	Count     int       `json:"count"` // 拉取到的节点数（过滤前）
	Used      int       `json:"used"`  // 过滤后实际使用的节点数
	Error     string    `json:"error,omitempty"`
	Stale     bool      `json:"stale"`             // 拉取失败，使用了旧缓存
	Hosts     int       `json:"hosts,omitempty"`   // 带过来的上游 hosts 条数（非底板来源的节点地址经 hosts 映射时才有）
	FetchedAt time.Time `json:"fetchedAt"`         // 所用内容（缓存）的拉取时间，手动节点没有
	ExpiresAt time.Time `json:"expiresAt"`         // 这份缓存的过期时间 = 拉取时间 + 缓存时长
	NoCache   bool      `json:"noCache,omitempty"` // 只用缓存时，这个来源还没有可用的缓存
}

// SourceState 是一个节点来源最近一次实际请求上游的结果（命中缓存的不算）。
// 只在内存里：重启后从磁盘缓存恢复成功的那部分。
type SourceState struct {
	Time     time.Time `json:"time"`               // 最近一次请求上游的时间
	Error    string    `json:"error,omitempty"`    // 最近一次请求失败的原因，成功则为空
	OKTime   time.Time `json:"okTime,omitzero"`    // 最近一次成功的时间
	Count    int       `json:"count"`              // 最近一次成功拿到的节点数
	Userinfo string    `json:"userinfo,omitempty"` // 最近一次成功时上游的 Subscription-Userinfo（流量、到期时间）
	Groups   []string  `json:"groups,omitempty"`   // 最近一次成功时上游带的策略组名：给前置代理提供候选
}

// stateOf 是一份成功拉取的结果对应的 SourceState。
func stateOf(e *cacheEntry) SourceState {
	st := SourceState{Time: e.FetchedAt, OKTime: e.FetchedAt, Count: len(e.Proxies), Userinfo: e.Userinfo}
	for _, g := range e.groups() {
		st.Groups = append(st.Groups, fmt.Sprint(g["name"]))
	}
	return st
}

type failure struct {
	at  time.Time
	err error
}

// Fetcher 按「来源 + 链接 + UA + 解锁地址」缓存上游结果，内存与磁盘各一份。
// 来源是全局的，所以引用同一个来源的订阅组共用缓存。
type Fetcher struct {
	mu    sync.Mutex
	dir   string
	cache map[string]*cacheEntry
	fails map[string]failure
	state map[string]SourceState // 来源 ID → 最近一次拉取的结果
	locks map[string]*sync.Mutex // 每个缓存键一把锁，避免并发请求重复拉取上游
}

func NewFetcher(dataDir string) *Fetcher {
	f := &Fetcher{
		dir:   filepath.Join(dataDir, "cache"),
		cache: map[string]*cacheEntry{}, fails: map[string]failure{}, locks: map[string]*sync.Mutex{},
		state: map[string]SourceState{},
	}
	_ = os.MkdirAll(f.dir, 0o755)
	files, _ := filepath.Glob(filepath.Join(f.dir, "*.json"))
	for _, path := range files {
		var e cacheEntry
		b, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(b, &e) != nil || e.Key+".json" != filepath.Base(path) {
			_ = os.Remove(path) // 损坏或旧格式的缓存
			continue
		}
		f.cache[e.Key] = &e
		if e.FetchedAt.After(f.state[e.SubID].OKTime) {
			f.state[e.SubID] = stateOf(&e)
		}
	}
	return f
}

// States 返回各来源最近一次拉取结果的副本。
func (f *Fetcher) States() map[string]SourceState {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]SourceState, len(f.state))
	for id, s := range f.state {
		out[id] = s
	}
	return out
}

// cacheKey 由所有影响拉取结果的输入算出。解锁 UA 只在有解锁地址时才起作用，所以也只在那时计入。
func cacheKey(subID, url, ua, unlock, unlockUA string) string {
	s := url + "\n" + ua + "\n" + unlock
	if strings.TrimSpace(unlock) != "" {
		s += "\n" + unlockUA
	}
	h := sha256.Sum256([]byte(s))
	return subID + "-" + hex.EncodeToString(h[:6])
}

func (f *Fetcher) keyLock(key string) *sync.Mutex {
	f.mu.Lock()
	defer f.mu.Unlock()
	l := f.locks[key]
	if l == nil {
		l = &sync.Mutex{}
		f.locks[key] = l
	}
	return l
}

func (f *Fetcher) store(e *cacheEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cache[e.Key] = e
	delete(f.fails, e.Key)
	f.state[e.SubID] = stateOf(e)
	if b, err := json.Marshal(e); err == nil {
		_ = os.WriteFile(filepath.Join(f.dir, e.Key+".json"), b, 0o600)
	}
	// 改过链接、UA 后旧的缓存还在，超出上限就淘汰最旧的
	for {
		var oldest *cacheEntry
		n := 0
		for _, c := range f.cache {
			if c.SubID != e.SubID {
				continue
			}
			n++
			if oldest == nil || c.FetchedAt.Before(oldest.FetchedAt) {
				oldest = c
			}
		}
		if n <= maxEntriesPerSub {
			return
		}
		delete(f.cache, oldest.Key)
		delete(f.locks, oldest.Key)
		_ = os.Remove(filepath.Join(f.dir, oldest.Key+".json"))
	}
}

// Get 以给定的链接和 UA 拉取订阅；unlockUA 是访问解锁地址用的 UA。
// 缓存未过期时直接返回缓存；拉取失败时返回 (同一链接 + UA 的旧缓存或 nil, err)。
// 不会回退到其他链接 / UA 的结果：不同 UA 拿到的内容可能不同，某个 UA 失败就是失败。
// FetchCacheOnly 只返回同一个键、当前格式的缓存（不管是否过期），没有就返回 nil，不请求上游。
func (f *Fetcher) Get(sub Source, url, ua, unlockUA string, ttl time.Duration, mode FetchMode) (*cacheEntry, error) {
	key := cacheKey(sub.ID, url, ua, sub.UnlockURL, unlockUA)
	if mode == FetchCacheOnly {
		f.mu.Lock()
		defer f.mu.Unlock()
		if e := f.cache[key]; e != nil && e.Format == cacheFormat {
			return e, nil
		}
		return nil, nil
	}
	l := f.keyLock(key)
	l.Lock()
	defer l.Unlock()

	f.mu.Lock()
	cached, fail := f.cache[key], f.fails[key]
	f.mu.Unlock()

	if mode != FetchRefresh {
		if cached != nil && cached.Format == cacheFormat && time.Since(cached.FetchedAt) < ttl {
			return cached, nil
		}
		if fail.err != nil && time.Since(fail.at) < failBackoff {
			return cached, fail.err
		}
	}
	up, userinfo, err := fetchSubscription(url, ua, sub.UnlockURL, unlockUA)
	if err != nil {
		f.mu.Lock()
		f.fails[key] = failure{time.Now(), err}
		st := f.state[sub.ID] // 保留上次成功的时间和节点数
		st.Time, st.Error = time.Now(), err.Error()
		f.state[sub.ID] = st
		f.mu.Unlock()
		return cached, err
	}
	e := &cacheEntry{Format: cacheFormat, Key: key, SubID: sub.ID, Userinfo: userinfo, FetchedAt: time.Now(), upstream: *up}
	f.store(e)
	return e, nil
}

// proxySummary 返回一行说明：上游拉取实际会用哪些代理环境变量（代理地址里的密码不打印）。
func proxySummary() string {
	redact := func(v string) string {
		if u, err := url.Parse(v); err == nil && u.Host != "" {
			return u.Redacted()
		}
		if i := strings.LastIndex(v, "@"); i >= 0 {
			return "***@" + v[i+1:]
		}
		return v
	}
	var parts []string
	proxied := false
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"} {
		v := strings.TrimSpace(firstNonEmpty(os.Getenv(k), os.Getenv(strings.ToLower(k))))
		if v == "" {
			continue
		}
		if k != "NO_PROXY" {
			proxied = true
			v = redact(v)
		}
		parts = append(parts, k+"="+v)
	}
	if !proxied {
		return "上游拉取：直连（未设置 HTTP_PROXY / HTTPS_PROXY）"
	}
	return "上游拉取走代理：" + strings.Join(parts, "，")
}

func fetchSubscription(url, ua, unlockURL, unlockUA string) (*upstream, string, error) {
	// 没有自定义 Transport，落在 http.DefaultTransport 上，它会读 HTTP_PROXY / HTTPS_PROXY / NO_PROXY。
	// 以后换成自定义 Transport 时要带上 Proxy: http.ProxyFromEnvironment，否则代理失效（TestFetchUsesProxy 会发现）。
	client := &http.Client{Timeout: 30 * time.Second}
	get := func(u, ua, accept string) (*http.Response, error) {
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", ua)
		req.Header.Set("Accept", accept)
		return client.Do(req)
	}

	// 解锁只是一次普通的 GET（默认用 Chrome 的 UA）：上游要求该地址被访问过之后才放行订阅拉取。
	// 两次请求互相独立，不共享 Cookie。
	if u := strings.TrimSpace(unlockURL); u != "" {
		resp, err := get(u, unlockUA, "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		if err != nil {
			return nil, "", fmt.Errorf("访问解锁地址失败: %w", err)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
	}

	resp, err := get(url, ua, "*/*")
	if err != nil {
		return nil, "", fmt.Errorf("拉取订阅失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("拉取订阅失败: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, "", fmt.Errorf("读取订阅失败: %w", err)
	}
	up, err := parseSubscription(body)
	if err != nil {
		return nil, "", err
	}
	return up, resp.Header.Get("Subscription-Userinfo"), nil
}

// parseSubscription 支持 Clash YAML 以及 base64 / 明文的分享链接列表。
// YAML 除了节点，把其余内容整份留在 Doc 里（键统一转成字符串，才能存进 JSON 缓存）。
func parseSubscription(body []byte) (*upstream, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(body, &doc); err == nil {
		up := &upstream{Proxies: mapsOf(doc["proxies"], true)}
		if len(up.Proxies) > 0 {
			delete(doc, "proxies")
			up.Doc = stringKeys(doc).(map[string]any)
			return up, nil
		}
	}
	text := strings.TrimSpace(string(body))
	if !strings.Contains(text, "://") {
		if dec, ok := decodeBase64(text); ok {
			text = dec
		}
	}
	if proxies := parseURIList(text); len(proxies) > 0 {
		return &upstream{Proxies: proxies}, nil
	}
	return nil, errors.New("订阅内容中没有可识别的节点（需要 Clash YAML 或分享链接列表）")
}

// stringKeys 把 YAML 里键不全是字符串的对象（yaml.v3 解析成 map[any]any）转成 map[string]any。
func stringKeys(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = stringKeys(x)
		}
		return t
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[fmt.Sprint(k)] = stringKeys(x)
		}
		return m
	case []any:
		for i, x := range t {
			t[i] = stringKeys(x)
		}
	}
	return v
}

// mapsOf 取出列表里的对象；named 为真时只要带 name 的。
func mapsOf(v any, named bool) []map[string]any {
	list, _ := v.([]any)
	var out []map[string]any
	for _, it := range list {
		m, _ := it.(map[string]any)
		if name, _ := m["name"].(string); m == nil || (named && name == "") {
			continue
		}
		out = append(out, m)
	}
	return out
}

// stringsOf 取出列表里的非空字符串。
func stringsOf(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, it := range list {
		if s, ok := it.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

func decodeBase64(s string) (string, bool) {
	s = strings.NewReplacer("\r", "", "\n", "", " ", "").Replace(s)
	s = strings.TrimRight(s, "=")
	for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return string(b), true
		}
	}
	return "", false
}

func parseURIList(text string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if p := parseURI(line); p != nil {
			out = append(out, p)
		}
	}
	return out
}
