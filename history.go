package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 历史版本：每次保存配置且内容有变化时，把被替换掉的那一份存到 DATA_DIR/history/，只留最近 maxHistory 份。
// 界面上的「套用模板」「删除来源」和整份导入都是直接覆盖，这是它们的后悔药。
// 文件里有订阅链接和令牌（和 config.json 一样），不含管理密码。

const maxHistory = 10

// snapshotFile 是一份历史版本文件的内容。
type snapshotFile struct {
	Time   time.Time `json:"time"` // 这份配置被替换掉的时间
	Config Config    `json:"config"`
}

// Snapshot 是历史版本列表里的一项。
type Snapshot struct {
	ID       string    `json:"id"` // 文件名（不含扩展名）
	Time     time.Time `json:"time"`
	Profiles []string  `json:"profiles"` // 当时的订阅组名称
	Sources  int       `json:"sources"`
}

func (s *Store) historyDir() string { return filepath.Join(filepath.Dir(s.path), "history") }

// snapshotIDs 返回现有历史版本的 ID，最新的在前。文件名以时间开头、版本号补零结尾，按字典序就是时间顺序。
func (s *Store) snapshotIDs() []string {
	files, _ := filepath.Glob(filepath.Join(s.historyDir(), "*.json"))
	ids := make([]string, 0, len(files))
	for _, f := range files {
		ids = append(ids, strings.TrimSuffix(filepath.Base(f), ".json"))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids
}

// snapshot 把被替换掉的配置存成一份历史版本。调用方持有写锁。失败只是少一份备份，不影响保存。
func (s *Store) snapshot(old Config) {
	now := time.Now()
	b, err := json.Marshal(snapshotFile{Time: now, Config: old})
	if err != nil || os.MkdirAll(s.historyDir(), 0o755) != nil {
		return
	}
	name := fmt.Sprintf("%s-%09d.json", now.Format("20060102-150405"), old.Revision)
	_ = os.WriteFile(filepath.Join(s.historyDir(), name), b, 0o600)
	if ids := s.snapshotIDs(); len(ids) > maxHistory {
		for _, id := range ids[maxHistory:] {
			_ = os.Remove(filepath.Join(s.historyDir(), id+".json"))
		}
	}
}

func (s *Store) readSnapshot(id string) (snapshotFile, bool) {
	var f snapshotFile
	b, err := os.ReadFile(filepath.Join(s.historyDir(), id+".json"))
	return f, err == nil && json.Unmarshal(b, &f) == nil
}

// History 列出历史版本，最新的在前。
func (s *Store) History() []Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := []Snapshot{}
	for _, id := range s.snapshotIDs() {
		f, ok := s.readSnapshot(id)
		if !ok {
			continue
		}
		item := Snapshot{ID: id, Time: f.Time, Profiles: []string{}, Sources: len(f.Config.Sources)}
		for _, p := range f.Config.Profiles {
			item.Profiles = append(item.Profiles, p.Name)
		}
		list = append(list, item)
	}
	return list
}

// Snapshot 读出一份历史版本的配置。id 只认列表里现有的，所以不会被用来读别的文件。
func (s *Store) Snapshot(id string) (Config, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, x := range s.snapshotIDs() {
		if x == id {
			f, ok := s.readSnapshot(id)
			return f.Config, ok
		}
	}
	return Config{}, false
}

func (a *App) handleGetHistory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"items": a.store.History()})
}

// handleRestoreHistory 用一份历史版本替换当前配置，和导入整份配置走同一条路：令牌以那份历史里的为准。
// 当前配置会先成为新的一份历史版本，所以恢复本身也能撤销。
func (a *App) handleRestoreHistory(w http.ResponseWriter, r *http.Request) {
	var req struct{ ID string }
	if !readJSON(w, r, &req) {
		return
	}
	old, ok := a.store.Snapshot(req.ID)
	if !ok {
		writeErr(w, http.StatusNotFound, "这份历史版本不存在")
		return
	}
	c, err := importConfig(old)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "恢复失败："+err.Error())
		return
	}
	a.saveConfig(w, c, -1)
}
