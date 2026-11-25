package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// 拉取 UA 跟随 Clash Verge Rev 的最新正式版（Config.FollowUA）：定期查 GitHub 上最新 release 的 tag，
// 拉取上游时用 clash-verge/<tag>。查到的版本存在 DATA_DIR/ua-version.json，不属于配置，重启后直接沿用。
const (
	clashVergeUAPrefix = "clash-verge/"
	uaCheckInterval    = 30 * 24 * time.Hour // 上次成功之后多久再查：版本号不用跟得很紧，一个月一次足够
	uaRetryInterval    = 24 * time.Hour      // 查询失败之后多久重试（界面上可以随时手动检查）
	uaLoopInterval     = time.Hour           // 后台多久看一次是否到期
)

// clashVergeLatest 会跳转到最新正式版（不含预发布版）的 release 页面
const clashVergeLatest = "https://github.com/clash-verge-rev/clash-verge-rev/releases/latest"

// versionTag 是可以接受的版本号 tag：v2.5.10 这样的数字版本。
var versionTag = regexp.MustCompile(`^v\d+(\.\d+){1,3}$`)

// UAVersion 是最近一次查询最新版本的结果。
type UAVersion struct {
	Version   string    `json:"version"`            // 最近一次查到的版本（如 v2.5.10），从没成功过为空
	OKAt      time.Time `json:"okAt,omitzero"`      // 最近一次成功的时间
	CheckedAt time.Time `json:"checkedAt,omitzero"` // 最近一次查询的时间（含失败）
	Error     string    `json:"error,omitempty"`    // 最近一次查询失败的原因，成功则为空
}

// UA 是按查到的版本拼出的 UA，没有版本时为空。
func (v UAVersion) UA() string {
	if v.Version == "" {
		return ""
	}
	return clashVergeUAPrefix + v.Version
}

// due 判断现在是否该查一次：从没成功过或成功已超过 uaCheckInterval，且距上次失败超过 uaRetryInterval。
func (v UAVersion) due(now time.Time) bool {
	if v.Version != "" && now.Sub(v.OKAt) < uaCheckInterval {
		return false
	}
	return v.Error == "" || now.Sub(v.CheckedAt) >= uaRetryInterval
}

type UAFollower struct {
	path  string
	api   string     // releases/latest 的地址，测试里换成本地的
	check sync.Mutex // 同一时间只查一次

	mu sync.Mutex
	v  UAVersion
}

func NewUAFollower(dataDir string) *UAFollower {
	u := &UAFollower{path: filepath.Join(dataDir, "ua-version.json"), api: clashVergeLatest}
	if b, err := os.ReadFile(u.path); err == nil {
		_ = json.Unmarshal(b, &u.v)
	}
	if !versionTag.MatchString(u.v.Version) {
		u.v.Version = ""
	}
	return u
}

// State 返回最近一次查询的结果。
func (u *UAFollower) State() UAVersion {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.v
}

// Check 查一次最新版本。失败时保留之前查到的版本，只记下错误。
func (u *UAFollower) Check() UAVersion {
	u.check.Lock()
	defer u.check.Unlock()
	tag, err := latestTag(u.api)
	u.mu.Lock()
	defer u.mu.Unlock()
	now := time.Now()
	u.v.CheckedAt = now
	if err != nil {
		u.v.Error = err.Error()
		log.Printf("查询 Clash Verge Rev 最新版本失败: %v", err)
	} else {
		if tag != u.v.Version {
			log.Printf("Clash Verge Rev 最新版本：%s，拉取 UA 改为 %s", tag, clashVergeUAPrefix+tag)
		}
		u.v.Version, u.v.OKAt, u.v.Error = tag, now, ""
	}
	if b, err := json.Marshal(u.v); err == nil {
		_ = os.WriteFile(u.path, b, 0o600)
	}
	return u.v
}

// CheckIfDue 在该查的时候查一次（见 UAVersion.due）。
func (u *UAFollower) CheckIfDue() {
	if u.State().due(time.Now()) {
		u.Check()
	}
}

// latestTag 从 releases/latest 的跳转地址（…/releases/tag/<tag>）里取出最新正式版的 tag。
// 不用 api.github.com：未登录的接口按 IP 限每小时 60 次，共用出口 IP 时很容易被拒。
func latestTag(page string) (string, error) {
	// 和拉取订阅一样落在 http.DefaultTransport 上，走 HTTP_PROXY / HTTPS_PROXY
	client := &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequest(http.MethodHead, page, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "script-merge")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode/100 != 3 || loc == "" {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	_, tag, ok := strings.Cut(loc, "/releases/tag/")
	if tag, _ = url.PathUnescape(tag); !ok || !versionTag.MatchString(tag) {
		return "", errors.New("无法识别的版本号：" + loc)
	}
	return tag, nil
}
