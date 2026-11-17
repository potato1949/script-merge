package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// 第三方规则集的平铺：生成订阅时由服务端下载规则文件，逐行识别类型，展开成带策略的普通规则写进 rules，
// 和机场的配置一样，不再输出 rule-providers。这样客户端不用自己去 GitHub 下载（国内经常失败），
// 规则集是什么类型、什么格式、要不要 no-resolve 也都不用用户操心。

const (
	ruleTTL         = 24 * time.Hour  // 规则文件的缓存时间，各规则仓库一般一天更新一次
	ruleFailBackoff = 5 * time.Minute // 下载失败后，同一地址在这段时间内不再重试
	ruleMaxBytes    = 32 << 20
	ruleWarnTotal   = 50000 // 规则总数超过这么多时提醒：平铺的规则客户端要逐条匹配，太多会变慢
)

// ruleDownload 下载一个规则文件，测试里会换掉（测试不访问外网）。
// 和上游订阅一样用默认的 Transport，走环境变量里的代理。
var ruleDownload = func(url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "script-merge")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, ruleMaxBytes))
}

// ruleEntry 是一个规则文件解析后的结果。
type ruleEntry struct {
	URL       string    `json:"url"`
	Rules     []string  `json:"rules"`   // 规范化后的规则，不带策略：DOMAIN-SUFFIX,google.com、IP-CIDR,1.0.0.0/8
	Skipped   int       `json:"skipped"` // 认不出而跳过的行数
	FetchedAt time.Time `json:"fetchedAt"`
}

// RuleFetcher 按地址缓存规则文件，内存与磁盘（DATA_DIR/rulecache）各一份。磁盘上的只在用到时才读进内存。
type RuleFetcher struct {
	mu    sync.Mutex
	dir   string
	cache map[string]*ruleEntry
	fails map[string]failure
	locks map[string]*sync.Mutex
}

func NewRuleFetcher(dir string) *RuleFetcher {
	_ = os.MkdirAll(dir, 0o755)
	return &RuleFetcher{dir: dir, cache: map[string]*ruleEntry{}, fails: map[string]failure{}, locks: map[string]*sync.Mutex{}}
}

func (f *RuleFetcher) path(url string) string {
	h := sha256.Sum256([]byte(url))
	return filepath.Join(f.dir, hex.EncodeToString(h[:8])+".json")
}

// Get 返回规则文件的内容。缓存一天内直接用；下载失败时返回 (旧缓存或 nil, err)。
func (f *RuleFetcher) Get(url string) (*ruleEntry, error) {
	f.mu.Lock()
	l := f.locks[url]
	if l == nil {
		l = &sync.Mutex{}
		f.locks[url] = l
	}
	f.mu.Unlock()
	l.Lock()
	defer l.Unlock()

	f.mu.Lock()
	cached, fail := f.cache[url], f.fails[url]
	f.mu.Unlock()
	if cached == nil {
		var e ruleEntry
		if b, err := os.ReadFile(f.path(url)); err == nil && json.Unmarshal(b, &e) == nil && e.URL == url {
			cached = &e
		}
	}
	if cached != nil && time.Since(cached.FetchedAt) < ruleTTL {
		f.remember(url, cached)
		return cached, nil
	}
	if fail.err != nil && time.Since(fail.at) < ruleFailBackoff {
		return cached, fail.err
	}
	e, err := downloadRules(url)
	if err != nil {
		f.mu.Lock()
		f.fails[url] = failure{time.Now(), err}
		f.mu.Unlock()
		return cached, err
	}
	f.remember(url, e)
	if b, err := json.Marshal(e); err == nil {
		_ = os.WriteFile(f.path(url), b, 0o600)
	}
	return e, nil
}

func (f *RuleFetcher) remember(url string, e *ruleEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cache[url] = e
	delete(f.fails, url)
}

func downloadRules(url string) (*ruleEntry, error) {
	if strings.HasSuffix(strings.ToLower(url), ".mrs") {
		return nil, errors.New("mrs 格式（二进制）无法展开成规则，请改用这条规则的 .list 或 .yaml 地址")
	}
	body, err := ruleDownload(url)
	if err != nil {
		return nil, err
	}
	rules, skipped, err := parseRuleFile(body)
	if err != nil {
		return nil, err
	}
	return &ruleEntry{URL: url, Rules: rules, Skipped: skipped, FetchedAt: time.Now()}, nil
}

// parseRuleFile 解析规则文件：YAML（payload 列表）或每行一条的文本。
// 每一行可以是完整规则（classical）、域名（domain）或 IP 段（ipcidr），按内容逐行判断，所以混在一起也没关系。
func parseRuleFile(body []byte) ([]string, int, error) {
	if bytes.HasPrefix(body, []byte{0x28, 0xb5, 0x2f, 0xfd}) || bytes.IndexByte(body[:min(len(body), 512)], 0) >= 0 {
		return nil, 0, errors.New("不是文本格式（可能是 mrs），无法展开成规则")
	}
	var items []string
	var doc map[string]any
	if yaml.Unmarshal(body, &doc) == nil && doc["payload"] != nil {
		list, _ := doc["payload"].([]any)
		for _, it := range list {
			items = append(items, fmt.Sprint(it))
		}
	} else {
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") || line == "payload:" {
				continue
			}
			items = append(items, strings.TrimSpace(strings.TrimPrefix(line, "- ")))
		}
	}
	rules, skipped := []string{}, 0
	for _, it := range items {
		if r, ok := normRule(it); ok {
			rules = append(rules, r)
		} else {
			skipped++
		}
	}
	if len(rules) == 0 {
		return nil, skipped, errors.New("文件里没有认得出的规则")
	}
	return rules, skipped, nil
}

// ruleTypes 是可以出现在规则集里的规则类型（mihomo 支持的）。认不出的类型直接跳过：
// 平铺之后一条认不出的规则会让客户端整个配置加载失败，而在 rule-provider 里它只是被忽略。
var ruleTypes = map[string]bool{
	"DOMAIN": true, "DOMAIN-SUFFIX": true, "DOMAIN-KEYWORD": true, "DOMAIN-REGEX": true, "DOMAIN-WILDCARD": true, "GEOSITE": true,
	"IP-CIDR": true, "IP-CIDR6": true, "IP-SUFFIX": true, "IP-ASN": true, "GEOIP": true,
	"SRC-GEOIP": true, "SRC-IP-ASN": true, "SRC-IP-CIDR": true, "SRC-IP-SUFFIX": true,
	"DST-PORT": true, "SRC-PORT": true, "IN-PORT": true, "IN-TYPE": true, "IN-USER": true, "IN-NAME": true,
	"PROCESS-NAME": true, "PROCESS-NAME-REGEX": true, "PROCESS-PATH": true, "PROCESS-PATH-REGEX": true,
	"UID": true, "NETWORK": true, "DSCP": true, "AND": true, "OR": true, "NOT": true,
}

// ipRuleTypes 是按目标 IP 匹配的规则：放在兜底规则之前，一律加 no-resolve，免得为了匹配它们去解析域名。
var ipRuleTypes = map[string]bool{"IP-CIDR": true, "IP-CIDR6": true, "IP-SUFFIX": true, "IP-ASN": true, "GEOIP": true}

var logicRuleTypes = map[string]bool{"AND": true, "OR": true, "NOT": true}

// ruleParams 是规则末尾可以跟的参数（在策略后面）。
var ruleParams = map[string]bool{"no-resolve": true, "src": true}

// normRule 把规则文件里的一项规范化成不带策略的规则；认不出返回 false。
func normRule(s string) (string, bool) {
	s = strings.TrimSpace(strings.Trim(strings.TrimSpace(s), `'"`))
	if s == "" || strings.ContainsAny(s, " \t") && !strings.Contains(s, ",") {
		return "", false
	}
	if typ, rest, ok := strings.Cut(s, ","); ok { // 完整规则
		typ = strings.ToUpper(strings.TrimSpace(typ))
		if !ruleTypes[typ] {
			return "", false
		}
		if logicRuleTypes[typ] {
			return typ + "," + strings.TrimSpace(rest), true
		}
		parts := strings.Split(rest, ",")
		payload := strings.TrimSpace(parts[0])
		if payload == "" || !validPayload(typ, payload) {
			return "", false
		}
		out := typ + "," + payload
		for _, p := range parts[1:] { // 规则集里的规则不带策略，只可能跟着参数；其余的丢掉
			if p = strings.TrimSpace(p); ruleParams[p] {
				out += "," + p
			}
		}
		return out, true
	}
	if p, err := netip.ParsePrefix(s); err == nil { // IP 段
		if p.Addr().Is4() {
			return "IP-CIDR," + p.String(), true
		}
		return "IP-CIDR6," + p.String(), true
	}
	if a, err := netip.ParseAddr(s); err == nil {
		if a.Is4() {
			return "IP-CIDR," + a.String() + "/32", true
		}
		return "IP-CIDR6," + a.String() + "/128", true
	}
	if strings.ContainsAny(s, "/,:") { // 域名
		return "", false
	}
	switch {
	case strings.HasPrefix(s, "+."):
		return "DOMAIN-SUFFIX," + s[2:], len(s) > 2
	case strings.HasPrefix(s, "."): // 只匹配子域名；平铺后没有对应的写法，按后缀处理（多匹配了它自己）
		return "DOMAIN-SUFFIX," + s[1:], len(s) > 1
	case strings.Contains(s, "*"): // 通配符：一个 * 代表一级
		return "DOMAIN-REGEX,^" + strings.ReplaceAll(regexp.QuoteMeta(s), `\*`, `[^.]+`) + "$", true
	}
	return "DOMAIN," + s, true
}

// validPayload 检查客户端会严格解析的几类内容，写错的那一行跳过，免得拖垮整个配置。
func validPayload(typ, payload string) bool {
	switch typ {
	case "IP-CIDR", "IP-CIDR6", "SRC-IP-CIDR", "IP-SUFFIX", "SRC-IP-SUFFIX":
		_, err := netip.ParsePrefix(payload)
		return err == nil
	case "DOMAIN-REGEX", "PROCESS-NAME-REGEX", "PROCESS-PATH-REGEX":
		_, err := compileRegex(payload)
		return err == nil
	}
	return true
}

// ruleKey 是一条规则去掉策略和参数后的部分，用来去重：排在后面的相同规则永远不会被匹配到。
func ruleKey(rule string) string {
	typ, rest, _ := strings.Cut(rule, ",")
	if logicRuleTypes[typ] {
		if i := strings.LastIndex(rule, ","); i > 0 { // 逻辑规则的策略在最后
			return rule[:i]
		}
		return rule
	}
	payload, _, _ := strings.Cut(rest, ",")
	return typ + "," + payload
}

// withPolicy 给规范化后的规则加上策略，IP 类规则补上 no-resolve。
func withPolicy(rule, policy string) string {
	typ, rest, _ := strings.Cut(rule, ",")
	if logicRuleTypes[typ] {
		return rule + "," + policy
	}
	parts := strings.Split(rest, ",")
	out := typ + "," + parts[0] + "," + policy
	noResolve := false
	for _, p := range parts[1:] {
		out += "," + p
		noResolve = noResolve || p == "no-resolve"
	}
	if ipRuleTypes[typ] && !noResolve {
		out += ",no-resolve"
	}
	return out
}

// RuleSetStatus 描述一次构建中某个规则集的情况，预览页列出来。
type RuleSetStatus struct {
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Count     int       `json:"count"`             // 展开出的规则数（去重前）
	Skipped   int       `json:"skipped,omitempty"` // 认不出而跳过的行数
	Error     string    `json:"error,omitempty"`
	Stale     bool      `json:"stale,omitempty"` // 下载失败，用了旧缓存
	FetchedAt time.Time `json:"fetchedAt,omitzero"`
}

// fetchRuleSets 并发下载一批规则文件，返回与 urls 一一对应的结果和错误。
func (f *RuleFetcher) fetchAll(urls []string) ([]*ruleEntry, []error) {
	entries, errs := make([]*ruleEntry, len(urls)), make([]error, len(urls))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			entries[i], errs[i] = f.Get(u)
		}()
	}
	wg.Wait()
	return entries, errs
}

// ruleList 收集最终的规则，按 ruleKey 去掉排在后面的重复规则。
type ruleList struct {
	rules []any
	seen  map[string]bool
}

func (l *ruleList) add(rule string) bool {
	if l.seen == nil {
		l.seen = map[string]bool{}
	}
	k := ruleKey(rule)
	if l.seen[k] {
		return false
	}
	l.seen[k] = true
	l.rules = append(l.rules, rule)
	return true
}
