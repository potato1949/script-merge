package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// configVersion 是当前配置结构的版本，见 Config.migrate。
const configVersion = 3

// Subscription 是订阅组里的一个节点来源：远程订阅（remote）或手动填写的节点（manual）。
type Subscription struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"` // remote / manual；空等同于 remote
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Prefix  string `json:"prefix"`  // 节点名前缀
	Include string `json:"include"` // 节点名包含正则
	Exclude string `json:"exclude"` // 节点名排除正则

	// 仅 manual：Clash YAML 数组或每行一条分享链接
	Nodes string `json:"nodes"`

	// 仅 remote
	URL       string `json:"url"`
	UAMode    string `json:"uaMode"`    // fixed：用 UserAgent；passthrough：透传拉取合并链接的客户端 UA
	UserAgent string `json:"userAgent"` // 固定 UA；透传模式下客户端没带 UA 时的后备值
	// Variants 按客户端 UA 切换上游链接：自上而下第一条匹配的生效，都不匹配用 URL
	Variants  []URLVariant `json:"variants"`
	UnlockURL string       `json:"unlockUrl"` // 拉取订阅前先访问一次的地址
}

// URLVariant 是「客户端 UA 匹配 → 专用订阅链接」的一条映射。
type URLVariant struct {
	Match string `json:"match"` // 正则，不区分大小写，匹配客户端 UA
	URL   string `json:"url"`
}

// Group 是一个策略组（Clash 的 proxy-group）。
type Group struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`  // select / url-test / fallback / load-balance
	Extra    []string `json:"extra"` // 固定成员（其他策略组、DIRECT、REJECT、指定节点）
	Nodes    string   `json:"nodes"` // none / all / filter
	Filter   string   `json:"filter"`
	TestURL  string   `json:"testUrl"`
	Interval int      `json:"interval"`
}

// RuleSet 是一个第三方规则集（输出为 rule-provider + RULE-SET 规则）。
type RuleSet struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	Behavior  string `json:"behavior"` // classical / domain / ipcidr
	Format    string `json:"format"`   // yaml / text / mrs
	Policy    string `json:"policy"`
	NoResolve bool   `json:"noResolve"`
	Enabled   bool   `json:"enabled"`
}

// Rules 是一套规则：前置规则 → 规则集 → 后置规则。
type Rules struct {
	RulesBefore string    `json:"rulesBefore"`
	RuleSets    []RuleSet `json:"ruleSets"`
	RulesAfter  string    `json:"rulesAfter"`
}

// Profile 是一个订阅组：节点来源 + 策略组 + 规则 + Parser，对应一个输出链接。
type Profile struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Token         string         `json:"token"`
	FileName      string         `json:"fileName"`
	Subscriptions []Subscription `json:"subscriptions"`
	Groups        []Group        `json:"groups"`
	Rules
	Parser string `json:"parser"`

	LegacyCustomProxies string `json:"customProxies,omitempty"` // v3 起并入 Subscriptions（manual）
}

// Template 是可套用到订阅组的模板。套用是复制：之后模板和订阅组各改各的，互不影响，
// 所以服务端只负责保存，复制动作在前端完成。
type Template struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Kind   string  `json:"kind"` // groups / rules / parser，决定下面哪部分有效
	Groups []Group `json:"groups"`
	Rules
	Parser string `json:"parser"`
}

type Config struct {
	Version        int        `json:"version"`
	Profiles       []Profile  `json:"profiles"`
	Templates      []Template `json:"templates"`
	CacheMinutes   int        `json:"cacheMinutes"`
	UpdateInterval int        `json:"updateInterval"` // 小时，写入 profile-update-interval
	BaseConfig     string     `json:"baseConfig"`     // 所有订阅组共用

	// 以下为旧版字段，仅用于 migrate。
	LegacyToken         string         `json:"token,omitempty"`         // v1：单订阅组
	LegacyFileName      string         `json:"fileName,omitempty"`      // v1
	LegacySubscriptions []Subscription `json:"subscriptions,omitempty"` // v1
	LegacyCustomProxies string         `json:"customProxies,omitempty"` // v1
	LegacyGroups        []Group        `json:"groups,omitempty"`        // v1、v2：策略组各组共用
	LegacyRulesBefore   string         `json:"rulesBefore,omitempty"`   // v1、v2
	LegacyRuleSets      []RuleSet      `json:"ruleSets,omitempty"`      // v1、v2
	LegacyRulesAfter    string         `json:"rulesAfter,omitempty"`    // v1、v2
	LegacyParser        string         `json:"parser,omitempty"`        // v1、v2：通用 Parser
}

// migrate 把旧版配置升级到当前结构，返回是否有改动。
//
//	v1：只有一组订阅，字段都在顶层
//	v2：有 Profiles，但策略组 / 规则 / 通用 Parser 仍在顶层、各组共用
//	v3：策略组、规则、Parser 都在订阅组内；自定义节点并入订阅源；新增模板
func (c *Config) migrate() bool {
	if c.Version >= configVersion {
		return false
	}
	if len(c.Profiles) == 0 { // v1 → v2
		c.Profiles = []Profile{{
			ID: randHex(8), Name: "默认", Token: firstNonEmpty(c.LegacyToken, randHex(16)),
			FileName: firstNonEmpty(c.LegacyFileName, "merged"), Subscriptions: c.LegacySubscriptions,
			LegacyCustomProxies: c.LegacyCustomProxies,
		}}
	}
	// v2 → v3：把共用的策略组、规则复制进每个订阅组
	groups, _ := json.Marshal(c.LegacyGroups)
	sets, _ := json.Marshal(c.LegacyRuleSets)
	for i := range c.Profiles {
		p := &c.Profiles[i]
		_ = json.Unmarshal(groups, &p.Groups)
		_ = json.Unmarshal(sets, &p.RuleSets)
		p.RulesBefore, p.RulesAfter = c.LegacyRulesBefore, c.LegacyRulesAfter
		if p.Parser == "" { // 两者都有时无法自动合并文本，通用 Parser 只保留为模板
			p.Parser = c.LegacyParser
		}
		if p.LegacyCustomProxies != "" {
			p.Subscriptions = append(p.Subscriptions, Subscription{
				ID: randHex(8), Kind: "manual", Name: "手动节点", Enabled: true, Nodes: p.LegacyCustomProxies,
			})
			p.LegacyCustomProxies = ""
		}
	}
	c.Templates = seedTemplates()
	if c.LegacyParser != "" {
		c.Templates = append(c.Templates, Template{ID: randHex(8), Name: "原通用 Parser", Kind: "parser", Parser: c.LegacyParser})
	}
	c.LegacyToken, c.LegacyFileName, c.LegacySubscriptions, c.LegacyCustomProxies = "", "", nil, ""
	c.LegacyGroups, c.LegacyRulesBefore, c.LegacyRuleSets, c.LegacyRulesAfter, c.LegacyParser = nil, "", nil, "", ""
	c.Version = configVersion
	return true
}

type authData struct {
	Hash string `json:"hash"`
	Salt string `json:"salt"`
}

type fileData struct {
	Auth   authData `json:"auth"`
	Config Config   `json:"config"`
}

type Store struct {
	mu   sync.RWMutex
	path string
	data fileData
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func hashPassword(password, salt string) string {
	k, err := pbkdf2.Key(sha256.New, password, []byte(salt), 200000, 32)
	if err != nil {
		panic(err)
	}
	return hex.EncodeToString(k)
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "config.json")}
	b, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.data.Config = defaultConfig()
		return s, s.save()
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, errors.New("配置文件损坏: " + err.Error())
	}
	if s.data.Config.migrate() {
		// 迁移前留一份备份，迁移有问题时可手动恢复
		_ = os.WriteFile(s.path+".bak", b, 0o600)
		return s, s.save()
	}
	return s, nil
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Config 返回配置的深拷贝。
func (s *Store) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var c Config
	b, _ := json.Marshal(s.data.Config)
	_ = json.Unmarshal(b, &c)
	return c
}

func (s *Store) SetConfig(c Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.data.Config
	s.data.Config = c
	if err := s.save(); err != nil {
		s.data.Config = old
		return err
	}
	return nil
}

func (s *Store) HasPassword() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Auth.Hash != ""
}

func (s *Store) SetPassword(password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	salt := randHex(16)
	s.data.Auth = authData{Hash: hashPassword(password, salt), Salt: salt}
	return s.save()
}

func (s *Store) CheckPassword(password string) bool {
	s.mu.RLock()
	a := s.data.Auth
	s.mu.RUnlock()
	if a.Hash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashPassword(password, a.Salt)), []byte(a.Hash)) == 1
}

const defaultBase = `mixed-port: 7890
allow-lan: false
mode: rule
log-level: info
ipv6: false
external-controller: 127.0.0.1:9090
unified-delay: true
tcp-concurrent: true
dns:
  enable: true
  ipv6: false
  enhanced-mode: fake-ip
  fake-ip-range: 198.18.0.1/16
  fake-ip-filter:
    - '*.lan'
    - '*.local'
    - '+.msftconnecttest.com'
    - '+.msftncsi.com'
  default-nameserver:
    - 223.5.5.5
    - 119.29.29.29
  nameserver:
    - https://doh.pub/dns-query
    - https://dns.alidns.com/dns-query
`

// 内置模板共用的策略组名：规则模板引用这些名字，所以两套策略组模板里都有它们。
const (
	gProxy  = "🚀 节点选择"
	gAuto   = "♻️ 自动选择"
	gAI     = "🤖 AI 服务"
	gDirect = "🎯 全球直连"
	gReject = "🛑 广告拦截"
	gFinal  = "🐟 漏网之鱼"

	testURL = "https://www.gstatic.com/generate_204"
)

func bm7(name, policy string) RuleSet {
	return RuleSet{
		Name:     name,
		URL:      "https://raw.githubusercontent.com/blackmatrix7/ios_rule_script/master/rule/Clash/" + name + "/" + name + ".yaml",
		Behavior: "classical", Format: "yaml", Policy: policy, Enabled: true,
	}
}

func loyal(name, policy string, ipcidr bool) RuleSet {
	behavior := "domain"
	if ipcidr {
		behavior = "ipcidr"
	}
	return RuleSet{
		Name:     "ls-" + name,
		URL:      "https://raw.githubusercontent.com/Loyalsoldier/clash-rules/release/" + name + ".txt",
		Behavior: behavior, Format: "yaml", Policy: policy, NoResolve: ipcidr, Enabled: true,
	}
}

func tailGroups() []Group {
	return []Group{
		{Name: gDirect, Type: "select", Extra: []string{"DIRECT", gProxy}, Nodes: "none"},
		{Name: gReject, Type: "select", Extra: []string{"REJECT", "DIRECT"}, Nodes: "none"},
		{Name: gFinal, Type: "select", Extra: []string{gProxy, "DIRECT"}, Nodes: "none"},
	}
}

func basicGroups() []Group {
	return append([]Group{
		{Name: gProxy, Type: "select", Extra: []string{gAuto, "DIRECT"}, Nodes: "all"},
		{Name: gAuto, Type: "url-test", Nodes: "all", TestURL: testURL, Interval: 300},
		{Name: gAI, Type: "select", Extra: []string{gProxy}, Nodes: "all"},
	}, tailGroups()...)
}

func regionGroups() []Group {
	regions := [][2]string{
		{"🇭🇰 香港", `香港|🇭🇰|\bHK\b|Hong ?Kong`},
		{"🇯🇵 日本", `日本|🇯🇵|\bJP\b|Japan`},
		{"🇸🇬 新加坡", `新加坡|狮城|🇸🇬|\bSG\b|Singapore`},
		{"🇺🇸 美国", `美国|🇺🇸|\bUS\b|United ?States`},
	}
	var names []string
	var gs []Group
	for _, r := range regions {
		names = append(names, r[0])
		gs = append(gs, Group{Name: r[0], Type: "url-test", Nodes: "filter", Filter: r[1], TestURL: testURL, Interval: 300})
	}
	head := []Group{
		{Name: gProxy, Type: "select", Extra: append(append([]string{gAuto}, names...), "DIRECT"), Nodes: "all"},
		{Name: gAuto, Type: "url-test", Nodes: "all", TestURL: testURL, Interval: 300},
		{Name: gAI, Type: "select", Extra: append([]string{"🇺🇸 美国", "🇯🇵 日本", "🇸🇬 新加坡"}, gProxy), Nodes: "none"},
	}
	return append(append(head, gs...), tailGroups()...)
}

func basicRules() Rules {
	return Rules{
		RuleSets: []RuleSet{
			bm7("Lan", "DIRECT"), bm7("AdvertisingLite", gReject),
			bm7("OpenAI", gAI), bm7("Claude", gAI), bm7("Gemini", gAI),
			bm7("Telegram", gProxy), bm7("GitHub", gProxy), bm7("Google", gProxy), bm7("Global", gProxy),
			bm7("China", gDirect),
		},
		RulesAfter: "GEOIP,CN," + gDirect + "\nMATCH," + gFinal,
	}
}

// seedTemplates 返回内置模板。它们只在新建配置或迁移时写入一次，之后和用户自建的模板没有区别。
func seedTemplates() []Template {
	return []Template{
		{ID: randHex(8), Kind: "groups", Name: "基础策略组", Groups: basicGroups()},
		{ID: randHex(8), Kind: "groups", Name: "按地区分组", Groups: regionGroups()},
		{ID: randHex(8), Kind: "rules", Name: "常用规则（blackmatrix7）", Rules: basicRules()},
		{ID: randHex(8), Kind: "rules", Name: "精简规则（Loyalsoldier）", Rules: Rules{
			RuleSets: []RuleSet{
				loyal("private", "DIRECT", false), loyal("reject", gReject, false),
				loyal("proxy", gProxy, false), loyal("direct", gDirect, false),
				loyal("telegramcidr", gProxy, true), loyal("cncidr", gDirect, true),
			},
			RulesAfter: "GEOIP,CN," + gDirect + "\nMATCH," + gFinal,
		}},
		{ID: randHex(8), Kind: "parser", Name: "允许局域网连接", Parser: "yaml:\n  mix-object:\n    allow-lan: true\n    bind-address: '*'\n"},
	}
}

func defaultConfig() Config {
	return Config{
		Version: configVersion,
		Profiles: []Profile{{
			ID: randHex(8), Name: "默认", Token: randHex(16), FileName: "merged",
			Subscriptions: []Subscription{}, Groups: basicGroups(), Rules: basicRules(),
		}},
		Templates:      seedTemplates(),
		CacheMinutes:   5,
		UpdateInterval: 12,
		BaseConfig:     defaultBase,
	}
}
