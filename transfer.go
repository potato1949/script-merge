package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// exportFile 是导出文件的格式。Kind 为 config 时带整份配置（含订阅链接和令牌，用于备份和换服务器）；
// 为 profile 时带一个订阅组和它引用的节点来源（去掉了订阅链接、解锁地址、手填节点和令牌，用于分享）。
type exportFile struct {
	App        string    `json:"app"`
	Kind       string    `json:"kind"`
	Version    int       `json:"version"` // 导出时的 configVersion，导入时据此迁移
	ExportedAt time.Time `json:"exportedAt"`
	Config     *Config   `json:"config,omitempty"`
	Profile    *Profile  `json:"profile,omitempty"`
	Sources    []Source  `json:"sources,omitempty"`
}

// exportProfile 导出一个订阅组用于分享：只留结构，不带任何能拿到节点的内容。
func exportProfile(cfg Config, p Profile) exportFile {
	f := exportFile{App: "script-merge", Kind: "profile", Version: configVersion, ExportedAt: time.Now()}
	p.ID, p.Token, p.Applied = "", "", nil // 模板 ID 到了别处没有意义
	used := map[string]bool{}
	for _, r := range p.Sources {
		used[r.Source] = true
	}
	for _, s := range cfg.Sources {
		if used[s.ID] {
			s.URL, s.UnlockURL, s.Nodes = "", "", ""
			f.Sources = append(f.Sources, s)
		}
	}
	f.Profile = &p
	return f
}

func (a *App) handleExport(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Config()
	f := exportFile{App: "script-merge", Kind: "config", Version: configVersion, ExportedAt: time.Now(), Config: &cfg}
	name := "script-merge-" + f.ExportedAt.Format("20060102-150405")
	if id := r.URL.Query().Get("profile"); id != "" {
		p := findProfile(cfg, func(p Profile) bool { return p.ID == id })
		if p == nil {
			writeErr(w, http.StatusNotFound, "订阅组不存在")
			return
		}
		f = exportProfile(cfg, *p)
		name = "script-merge-" + strings.TrimSpace(p.Name)
	}
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name+".json"))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(f)
}

// validToken 判断导入文件里的令牌能否原样保留（它会出现在订阅链接的路径里）。
func validToken(t string) bool {
	const safe = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~"
	return t != "" && len(t) <= 128 && strings.Trim(t, safe) == ""
}

// importConfig 用导入的整份配置替换当前配置：先按文件里的版本迁移，再走和保存时一样的校验。
// 令牌以文件里的为准，这样换服务器后客户端里的订阅链接不用改。
func importConfig(in Config) (Config, error) {
	if in.Version > configVersion {
		return in, errors.New("这份配置来自更新的版本，请先升级本服务")
	}
	in.migrate()
	tokens, used := map[string]string{}, map[string]bool{}
	for _, p := range in.Profiles {
		if validID(p.ID) && validToken(p.Token) && !used[p.Token] {
			tokens[p.ID], used[p.Token] = p.Token, true
		}
	}
	return in, validateConfig(&in, tokens)
}

// importProfile 把导出的单个订阅组加进当前配置：它和它带来的来源都换上新 ID，订阅组得到新令牌。
func importProfile(cur Config, f exportFile) (Config, error) {
	if f.Version > configVersion {
		return cur, errors.New("这个文件来自更新的版本，请先升级本服务")
	}
	if f.Version < 5 { // 单个订阅组的导出从 v5 才有
		return cur, errors.New("文件版本无效")
	}
	tmp := Config{Version: f.Version, Profiles: []Profile{*f.Profile}, Sources: f.Sources}
	tmp.migrate()
	p := tmp.Profiles[0]
	p.ID, p.Token, p.Applied = "", "", nil
	ids := map[string]string{}
	for _, s := range tmp.Sources {
		if _, dup := ids[s.ID]; dup || s.ID == "" {
			continue
		}
		ids[s.ID] = randHex(8)
		s.ID = ids[s.ID]
		cur.Sources = append(cur.Sources, s)
	}
	refs := []SourceRef{}
	for _, r := range p.Sources {
		if id, ok := ids[r.Source]; ok {
			r.Source = id
			refs = append(refs, r)
		}
	}
	p.Sources = refs
	names := map[string]bool{}
	for _, x := range cur.Profiles {
		names[x.Name] = true
	}
	if p.Name = strings.TrimSpace(p.Name); p.Name == "" {
		p.Name = "导入的订阅组"
	}
	for names[p.Name] {
		p.Name += "（导入）"
	}
	tokens := map[string]string{}
	for _, x := range cur.Profiles {
		tokens[x.ID] = x.Token
	}
	cur.Profiles = append(cur.Profiles, p)
	return cur, validateConfig(&cur, tokens)
}

// handleImport 接受 /api/export 导出的两种文件，也接受直接上传的 config.json（{auth, config}，只取 config）
// 或不带外层的配置对象。整份配置会替换当前配置；单个订阅组是追加。
func (a *App) handleImport(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "文件太大或读取失败")
		return
	}
	var f exportFile
	if err := json.Unmarshal(body, &f); err != nil {
		writeErr(w, http.StatusBadRequest, "不是合法的 JSON 文件")
		return
	}
	var c Config
	switch {
	case f.Profile != nil:
		c, err = importProfile(a.store.Config(), f)
	case f.Config != nil:
		c, err = importConfig(*f.Config)
	default:
		var bare Config
		if json.Unmarshal(body, &bare) != nil || len(bare.Profiles)+len(bare.LegacySubscriptions) == 0 {
			writeErr(w, http.StatusBadRequest, "文件里没有可导入的配置")
			return
		}
		c, err = importConfig(bare)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "导入失败："+err.Error())
		return
	}
	a.saveConfig(w, c)
}
