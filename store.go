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
	"strings"
	"sync"
)

// configVersion 是当前配置结构的版本，见 Config.migrate。
const configVersion = 6

// Source 是一个全局的节点来源：远程订阅（remote）或手动填写的节点（manual）。
// 多个订阅组可以引用同一个来源（SourceRef），这里只放属于来源本身的内容。
type Source struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // remote / manual；空等同于 remote
	Name string `json:"name"`

	// 仅 manual：Clash YAML 数组或每行一条分享链接
	Nodes string `json:"nodes"`

	// 仅 remote
	URL       string `json:"url"`
	UAMode    string `json:"uaMode"`    // fixed：用全局的 Config.UserAgent；passthrough：透传拉取合并链接的客户端 UA
	UnlockURL string `json:"unlockUrl"` // 拉取订阅前先访问一次的地址
}

// SourceRef 是订阅组对一个节点来源的引用，带「本组怎么用它」的设置。
// 同一个来源可以在一个订阅组里被引用多次（比如用不同的过滤条件）。
type SourceRef struct {
	Source  string `json:"source"` // Source.ID
	Enabled bool   `json:"enabled"`
	Prefix  string `json:"prefix"`  // 节点名前缀
	Include string `json:"include"` // 节点名包含正则
	Exclude string `json:"exclude"` // 节点名排除正则
	// Dialer 是前置代理（代理链）：非空时本引用的节点都写上 dialer-proxy，经由这个策略组或节点拨号
	Dialer string `json:"dialer"`
}

// TemplateRef 记录订阅组某部分内容最近一次套用的模板，以及套用时的内容摘要（由前端计算）。
// 只用于界面上的差异提示，Build 不使用它。
type TemplateRef struct {
	ID   string `json:"id"`
	Hash string `json:"hash"`
}

// Subscription 是 v4 及以前的节点来源：来源本身和订阅组的使用设置混在一起，且各订阅组各存一份。
// v5 起拆成 Source + SourceRef，这个类型只用于 migrate。
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
	UAMode    string `json:"uaMode"`    // fixed：用全局的 Config.UserAgent；passthrough：透传拉取合并链接的客户端 UA
	UnlockURL string `json:"unlockUrl"` // 拉取订阅前先访问一次的地址

	LegacyVariants  []URLVariant `json:"variants,omitempty"`  // v4 起移除：按客户端 UA 切换上游链接
	LegacyUserAgent string       `json:"userAgent,omitempty"` // v4 起改为全局的 Config.UserAgent
}

// URLVariant 是旧版「客户端 UA 匹配 → 专用订阅链接」的一条映射，仅用于 migrate。
type URLVariant struct {
	Match string `json:"match"`
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

// RuleSource 是一个第三方规则仓库：规则编辑器里「从预设添加」的来源。
// 它只是添加规则集时的帮手——加进订阅组或模板的规则集存的是拼好的 URL，之后和来源互不影响，Build 也不使用它。
// 仓库里有哪些规则（规则清单）不属于配置，手动拉取后存在 RuleNameStore。
type RuleSource struct {
	ID   string `json:"id"`
	Name string `json:"name"`

	// 规则清单从哪里列：GitHub 仓库某个分支下的一个目录。Repo 为空表示不拉清单，只用置顶的规则名
	Repo   string `json:"repo"`   // owner/repo
	Ref    string `json:"ref"`    // 分支或标签
	Dir    string `json:"dir"`    // 规则所在目录，空为仓库根目录
	Layout string `json:"layout"` // file：目录下每个文件是一条规则；dir：每个子目录是一条规则
	Ext    string `json:"ext"`    // 仅 file：只列这个扩展名的文件，规则名是去掉扩展名后的文件名

	// 添加规则集时怎么填
	URL      string `json:"url"`      // 规则文件地址，{name} 代表规则名
	Behavior string `json:"behavior"` // classical / domain / ipcidr
	IPCIDR   string `json:"ipcidr"`   // 规则名匹配这个正则时类型改为 ipcidr（前端使用），空为不启用
	Format   string `json:"format"`   // yaml / text / mrs
	Prefix   string `json:"prefix"`   // 加在规则集名称前面，避免不同来源的同名规则冲突

	Pinned []string `json:"pinned"` // 置顶的规则名：排在候选列表最前面，没拉过清单时也能选
}

// Rules 是一套规则：前置规则 → 规则集 → 后置规则。
type Rules struct {
	RulesBefore string    `json:"rulesBefore"`
	RuleSets    []RuleSet `json:"ruleSets"`
	RulesAfter  string    `json:"rulesAfter"`
}

// Profile 是一个订阅组：引用的节点来源 + 策略组 + 规则 + Parser，对应一个输出链接。
type Profile struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Token    string      `json:"token"`
	FileName string      `json:"fileName"`
	Sources  []SourceRef `json:"sources"`
	Groups   []Group     `json:"groups"`
	Rules
	Parser string `json:"parser"`
	// Applied 按模板类型（groups / rules / parser）记录最近一次套用的模板
	Applied map[string]TemplateRef `json:"applied,omitempty"`

	LegacySubscriptions []Subscription `json:"subscriptions,omitempty"` // v5 起拆成全局的 Config.Sources + 本组的 Sources
	LegacyCustomProxies string         `json:"customProxies,omitempty"` // v3 起并入节点来源（manual）
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
	Version        int          `json:"version"`
	Profiles       []Profile    `json:"profiles"`
	Sources        []Source     `json:"sources"` // 节点来源，被各订阅组引用
	Templates      []Template   `json:"templates"`
	RuleSources    []RuleSource `json:"ruleSources"` // 第三方规则来源，规则编辑器里「从预设添加」用
	CacheMinutes   int          `json:"cacheMinutes"`
	UpdateInterval int          `json:"updateInterval"` // 小时，写入 profile-update-interval
	BaseConfig     string       `json:"baseConfig"`     // 所有订阅组共用
	// UserAgent 是拉取上游订阅用的固定 UA（透传模式下客户端没带 UA 时也用它），空则用 defaultUA
	UserAgent string `json:"userAgent"`
	// UnlockUserAgent 是访问解锁地址用的 UA，空则用 chromeUA
	UnlockUserAgent string `json:"unlockUserAgent"`

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
//	v4：移除按客户端 UA 切换上游链接的「专用链接」，每个订阅只有一个链接；固定 UA 从各订阅挪到全局
//	v5：节点来源从各订阅组里提出来成为全局对象，订阅组只保留引用和本组的过滤设置
//	v6：第三方规则来源从前端写死的预设变成配置里的 RuleSources，旧配置补上内置的几个
func (c *Config) migrate() bool {
	from := c.Version
	if from >= configVersion {
		return false
	}
	if from < 3 {
		c.migrateToV3()
	}
	if from < 4 {
		c.migrateToV4()
	}
	if from < 5 {
		c.migrateToV5()
	}
	if len(c.RuleSources) == 0 {
		c.RuleSources = seedRuleSources()
	}
	c.Version = configVersion
	return true
}

func (c *Config) migrateToV4() {
	for i := range c.Profiles {
		subs := c.Profiles[i].LegacySubscriptions
		for j := range subs {
			s := &subs[j]
			// 默认链接为空的订阅改用第一条有链接的专用链接，免得迁移后无链接可拉
			for _, v := range s.LegacyVariants {
				if strings.TrimSpace(s.URL) != "" {
					break
				}
				s.URL = strings.TrimSpace(v.URL)
			}
			s.LegacyVariants = nil
			// 全局固定 UA 取第一个填了 UA 的订阅的值（各订阅填得不一样时，其余的丢弃）
			if c.UserAgent == "" {
				c.UserAgent = strings.TrimSpace(s.LegacyUserAgent)
			}
			s.LegacyUserAgent = ""
		}
	}
	if c.UserAgent == defaultUA { // 和内置默认值相同就留空，以后默认值更新时跟着变
		c.UserAgent = ""
	}
}

const hexDigits = "0123456789abcdef"

// validID 判断是否是服务端生成的那种 ID（16 位十六进制）。
func validID(id string) bool { return len(id) == 16 && strings.Trim(id, hexDigits) == "" }

// splitSubscription 把旧的订阅拆成「来源本身」和「订阅组怎么用它」两部分。
func splitSubscription(s Subscription) (Source, SourceRef) {
	src := Source{ID: s.ID, Kind: "remote", Name: s.Name}
	if s.Kind == "manual" {
		src.Kind, src.Nodes = "manual", s.Nodes
	} else {
		src.URL, src.UnlockURL, src.UAMode = strings.TrimSpace(s.URL), strings.TrimSpace(s.UnlockURL), "fixed"
		if s.UAMode == "passthrough" {
			src.UAMode = "passthrough"
		}
	}
	return src, SourceRef{Source: s.ID, Enabled: s.Enabled, Prefix: s.Prefix, Include: s.Include, Exclude: s.Exclude}
}

// migrateToV5 把各订阅组里的订阅提成全局的节点来源。内容完全相同的（远程：链接 + UA 模式 + 解锁地址；
// 手动：节点文本）合并成一个，名称和 ID 取第一次出现的；没填链接 / 节点的不合并。
func (c *Config) migrateToV5() {
	c.Sources = []Source{}
	same := map[string]string{} // 内容 → 来源 ID
	ids := map[string]bool{}
	for i := range c.Profiles {
		p := &c.Profiles[i]
		p.Sources = []SourceRef{}
		for _, s := range p.LegacySubscriptions {
			src, ref := splitSubscription(s)
			key := ""
			switch {
			case src.Kind == "manual" && strings.TrimSpace(src.Nodes) != "":
				key = "manual\n" + src.Nodes
			case src.Kind == "remote" && src.URL != "":
				key = "remote\n" + src.URL + "\n" + src.UAMode + "\n" + src.UnlockURL
			}
			if id, ok := same[key]; ok && key != "" {
				ref.Source = id
			} else {
				if !validID(src.ID) || ids[src.ID] {
					src.ID = randHex(8)
				}
				ids[src.ID] = true
				c.Sources = append(c.Sources, src)
				if key != "" {
					same[key] = src.ID
				}
				ref.Source = src.ID
			}
			p.Sources = append(p.Sources, ref)
		}
		p.LegacySubscriptions = nil
	}
}

func (c *Config) migrateToV3() {
	if len(c.Profiles) == 0 { // v1 → v2
		c.Profiles = []Profile{{
			ID: randHex(8), Name: "默认", Token: firstNonEmpty(c.LegacyToken, randHex(16)),
			FileName: firstNonEmpty(c.LegacyFileName, "merged"), LegacySubscriptions: c.LegacySubscriptions,
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
			p.LegacySubscriptions = append(p.LegacySubscriptions, Subscription{
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

// seedRuleSources 返回内置的规则来源，和内置模板一样只在新建配置或迁移时写入一次。
// 置顶的是各仓库里常用的规则，没拉过规则清单时候选列表里就是它们。
func seedRuleSources() []RuleSource {
	const raw = "https://raw.githubusercontent.com/"
	return []RuleSource{
		{ID: randHex(8), Name: "blackmatrix7/ios_rule_script", Repo: "blackmatrix7/ios_rule_script", Ref: "master", Dir: "rule/Clash", Layout: "dir",
			URL: raw + "blackmatrix7/ios_rule_script/master/rule/Clash/{name}/{name}.yaml", Behavior: "classical", Format: "yaml",
			Pinned: []string{"OpenAI", "Claude", "Gemini", "Copilot", "Google", "YouTube", "Netflix", "Disney", "Spotify", "Telegram", "Twitter", "Facebook", "Instagram", "TikTok", "GitHub", "Microsoft", "OneDrive", "Apple", "Steam", "Epic", "PayPal", "Amazon", "Cloudflare", "Bahamut", "BiliBili", "Global", "GlobalMedia", "Proxy", "China", "ChinaMax", "ChinaMedia", "Lan", "Advertising", "AdvertisingLite", "Privacy", "Hijacking", "Game", "Download", "Speedtest"}},
		{ID: randHex(8), Name: "Loyalsoldier/clash-rules", Repo: "Loyalsoldier/clash-rules", Ref: "release", Layout: "file", Ext: ".txt",
			URL: raw + "Loyalsoldier/clash-rules/release/{name}.txt", Behavior: "domain", IPCIDR: "cidr$", Format: "yaml", Prefix: "ls-",
			Pinned: []string{"reject", "icloud", "apple", "google", "proxy", "direct", "private", "gfw", "tld-not-cn", "telegramcidr", "cncidr", "lancidr"}},
		{ID: randHex(8), Name: "ACL4SSR", Repo: "ACL4SSR/ACL4SSR", Ref: "master", Dir: "Clash", Layout: "file", Ext: ".list",
			URL: raw + "ACL4SSR/ACL4SSR/master/Clash/{name}.list", Behavior: "classical", Format: "text", Prefix: "acl-",
			Pinned: []string{"LocalAreaNetwork", "UnBan", "BanAD", "BanProgramAD", "GoogleCN", "Microsoft", "Apple", "Telegram", "ProxyMedia", "ProxyGFWlist", "ChinaDomain", "ChinaCompanyIp", "Download"}},
		{ID: randHex(8), Name: "MetaCubeX geosite（mrs，需 mihomo 内核）", Repo: "MetaCubeX/meta-rules-dat", Ref: "meta", Dir: "geo/geosite", Layout: "file", Ext: ".mrs",
			URL: raw + "MetaCubeX/meta-rules-dat/meta/geo/geosite/{name}.mrs", Behavior: "domain", Format: "mrs", Prefix: "geosite-",
			Pinned: []string{"cn", "geolocation-!cn", "private", "category-ads-all", "openai", "anthropic", "google", "youtube", "netflix", "telegram", "github", "microsoft", "apple", "steam", "bilibili"}},
		{ID: randHex(8), Name: "MetaCubeX geoip（mrs，需 mihomo 内核）", Repo: "MetaCubeX/meta-rules-dat", Ref: "meta", Dir: "geo/geoip", Layout: "file", Ext: ".mrs",
			URL: raw + "MetaCubeX/meta-rules-dat/meta/geo/geoip/{name}.mrs", Behavior: "ipcidr", Format: "mrs", Prefix: "geoip-",
			Pinned: []string{"cn", "private", "telegram", "google", "netflix", "cloudflare"}},
	}
}

func defaultConfig() Config {
	return Config{
		Version: configVersion,
		Profiles: []Profile{{
			ID: randHex(8), Name: "默认", Token: randHex(16), FileName: "merged",
			Sources: []SourceRef{}, Groups: basicGroups(), Rules: basicRules(),
		}},
		Sources:        []Source{},
		Templates:      seedTemplates(),
		RuleSources:    seedRuleSources(),
		CacheMinutes:   5,
		UpdateInterval: 12,
		BaseConfig:     defaultBase,
	}
}
