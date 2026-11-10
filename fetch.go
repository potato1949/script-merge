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

// cacheEntry 是某个订阅以某个「链接 + UA」拉取到的结果。
type cacheEntry struct {
	Key       string           `json:"key"`
	SubID     string           `json:"subId"`
	Proxies   []map[string]any `json:"proxies"`
	Userinfo  string           `json:"userinfo"`
	FetchedAt time.Time        `json:"fetchedAt"`
}

// SubStatus 描述一次构建中某个订阅的状态。
type SubStatus struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`   // 本次实际使用的上游链接
	UA        string    `json:"ua"`    // 本次实际使用的 UA
	Count     int       `json:"count"` // 拉取到的节点数（过滤前）
	Used      int       `json:"used"`  // 过滤后实际使用的节点数
	Error     string    `json:"error,omitempty"`
	Stale     bool      `json:"stale"` // 拉取失败，使用了旧缓存
	FetchedAt time.Time `json:"fetchedAt"`
}

type failure struct {
	at  time.Time
	err error
}

// Fetcher 按「订阅 + 链接 + UA + 解锁地址」缓存上游结果，内存与磁盘各一份。
type Fetcher struct {
	mu    sync.Mutex
	dir   string
	cache map[string]*cacheEntry
	fails map[string]failure
	locks map[string]*sync.Mutex // 每个缓存键一把锁，避免并发请求重复拉取上游
}

func NewFetcher(dataDir string) *Fetcher {
	f := &Fetcher{
		dir:   filepath.Join(dataDir, "cache"),
		cache: map[string]*cacheEntry{}, fails: map[string]failure{}, locks: map[string]*sync.Mutex{},
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
	}
	return f
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
	if b, err := json.Marshal(e); err == nil {
		_ = os.WriteFile(filepath.Join(f.dir, e.Key+".json"), b, 0o600)
	}
	// 透传 UA 时缓存份数由客户端决定，超出上限就淘汰最旧的
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
// 不会回退到其他链接 / UA 的结果：不同 UA 拿到的节点可能不同，某个 UA 失败就是失败。
func (f *Fetcher) Get(sub Subscription, url, ua, unlockUA string, ttl time.Duration, force bool) (*cacheEntry, error) {
	key := cacheKey(sub.ID, url, ua, sub.UnlockURL, unlockUA)
	l := f.keyLock(key)
	l.Lock()
	defer l.Unlock()

	f.mu.Lock()
	cached, fail := f.cache[key], f.fails[key]
	f.mu.Unlock()

	if !force {
		if cached != nil && time.Since(cached.FetchedAt) < ttl {
			return cached, nil
		}
		if fail.err != nil && time.Since(fail.at) < failBackoff {
			return cached, fail.err
		}
	}
	proxies, userinfo, err := fetchSubscription(url, ua, sub.UnlockURL, unlockUA)
	if err != nil {
		f.mu.Lock()
		f.fails[key] = failure{time.Now(), err}
		f.mu.Unlock()
		return cached, err
	}
	e := &cacheEntry{Key: key, SubID: sub.ID, Proxies: proxies, Userinfo: userinfo, FetchedAt: time.Now()}
	f.store(e)
	return e, nil
}

func fetchSubscription(url, ua, unlockURL, unlockUA string) ([]map[string]any, string, error) {
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
	proxies, err := parseSubscription(body)
	if err != nil {
		return nil, "", err
	}
	return proxies, resp.Header.Get("Subscription-Userinfo"), nil
}

// parseSubscription 支持 Clash YAML 以及 base64 / 明文的分享链接列表。
func parseSubscription(body []byte) ([]map[string]any, error) {
	var doc struct {
		Proxies []map[string]any `yaml:"proxies"`
	}
	if err := yaml.Unmarshal(body, &doc); err == nil && len(doc.Proxies) > 0 {
		var out []map[string]any
		for _, p := range doc.Proxies {
			if name, _ := p["name"].(string); name != "" {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	text := strings.TrimSpace(string(body))
	if !strings.Contains(text, "://") {
		if dec, ok := decodeBase64(text); ok {
			text = dec
		}
	}
	if proxies := parseURIList(text); len(proxies) > 0 {
		return proxies, nil
	}
	return nil, errors.New("订阅内容中没有可识别的节点（需要 Clash YAML 或分享链接列表）")
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
