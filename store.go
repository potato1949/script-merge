package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// configVersion 是当前配置结构的版本。不做迁移：版本对不上的旧配置直接作废（原文件留作 config.json.bak），
// 管理密码保留。v8 是「以机场配置为底板」的重构版本，此前的 v1–v7 是自建策略组和规则的结构。
const configVersion = 8

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
	UnlockURL string `json:"unlockUrl"` // 拉取订阅前先访问一次的地址
}

// SourceRef 是订阅组对一个节点来源的引用，带「本组怎么用它」的设置。
type SourceRef struct {
	Source  string `json:"source"` // Source.ID
	Enabled bool   `json:"enabled"`
	Prefix  string `json:"prefix"`  // 节点名前缀
	Include string `json:"include"` // 节点名包含正则
	Exclude string `json:"exclude"` // 节点名排除正则
	// Dialer 是前置代理（代理链）：非空时本引用的节点都写上 dialer-proxy，经由这个策略组或节点拨号
	Dialer string `json:"dialer"`
}

// Profile 是一个订阅组：以一个机场的完整配置为底板，并入其他来源的节点，对应一个输出链接。
type Profile struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Token    string `json:"token"`
	FileName string `json:"fileName"`
	// Base 是底板机场的来源 ID，必须是本组引用的远程来源。输出以它的完整 Clash 配置为准（见 build.go）
	Base    string      `json:"base"`
	Sources []SourceRef `json:"sources"`
	Parser  string      `json:"parser"`
}

type Config struct {
	Version int `json:"version"`
	// Revision 每保存一次加一。前端保存时带回它加载时的值，对不上说明配置已在别处被改过（见 SetConfig）
	Revision       int       `json:"revision"`
	Profiles       []Profile `json:"profiles"`
	Sources        []Source  `json:"sources"` // 节点来源，被各订阅组引用
	CacheMinutes   int       `json:"cacheMinutes"`
	UpdateInterval int       `json:"updateInterval"` // 小时，写入 Profile-Update-Interval 响应头
	// UserAgent 是拉取上游订阅用的 UA，空则用 defaultUA
	UserAgent string `json:"userAgent"`
	// FollowUA 为真时拉取 UA 跟随 Clash Verge Rev 的最新版本（见 uafollow.go），UserAgent 不起作用；
	// 还没查到版本时用 defaultUA
	FollowUA bool `json:"followUA"`
	// UnlockUserAgent 是访问解锁地址用的 UA，空则用 chromeUA
	UnlockUserAgent string `json:"unlockUserAgent"`
}

const hexDigits = "0123456789abcdef"

// validID 判断是否是服务端生成的那种 ID（16 位十六进制）。
func validID(id string) bool { return len(id) == 16 && strings.Trim(id, hexDigits) == "" }

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
	if v := s.data.Config.Version; v != configVersion {
		// 旧结构不迁移：原文件留一份备份，配置从头开始，管理密码保留
		log.Printf("配置文件是旧版本（%d），已作废并备份为 config.json.bak，请重新配置", v)
		_ = os.WriteFile(s.path+".bak", b, 0o600)
		s.data.Config = defaultConfig()
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

// errConflict：保存时带来的版本号和当前的对不上，配置已在别的页面或设备上被修改过。
var errConflict = errors.New("配置已在别的页面或设备上被修改过")

// SetConfig 保存配置，并把保存后的版本号写回 c.Revision。
// expect 是调用方加载配置时的版本号，对不上就返回 errConflict 而不保存；传负数表示不检查。
func (s *Store) SetConfig(c *Config, expect int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.data.Config
	if expect >= 0 && expect != old.Revision {
		return errConflict
	}
	c.Revision = old.Revision + 1
	s.data.Config = *c
	if err := s.save(); err != nil {
		s.data.Config = old
		c.Revision = old.Revision
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

func defaultConfig() Config {
	return Config{
		Version:        configVersion,
		Profiles:       []Profile{},
		Sources:        []Source{},
		CacheMinutes:   5,
		UpdateInterval: 12,
	}
}
