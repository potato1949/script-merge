package main

import (
	"regexp"
	"runtime/debug"
	"strings"
	"time"
)

// version 由编译参数指定：go build -ldflags "-X main.version=v1.2.0"。
// 没指定时从构建信息里取：当前提交正好打了版本 tag 就用 tag（Go 1.24 起 go build 自动带上），
// 否则用 git 提交号（go run 和不带 .git 的 Docker 构建都没有）。
var version string

// pseudoVersion 匹配 Go 伪版本的结尾（-时间戳-提交号），即当前提交不在 tag 上。
var pseudoVersion = regexp.MustCompile(`\d{14}-[0-9a-f]{12}$`)

// tagVersion 从构建信息的主模块版本里取 tag 版本号；不是正好在 tag 上时返回空。
func tagVersion(v string) string {
	v = strings.TrimSuffix(v, "+dirty")
	if v == "" || v == "(devel)" || pseudoVersion.MatchString(v) {
		return ""
	}
	return v
}

// BuildInfo 是界面上显示的程序版本。
type BuildInfo struct {
	Version  string    `json:"version"`            // 显示用：指定的版本号、tag，或提交号前 7 位，都没有则为空
	Commit   string    `json:"commit,omitempty"`   // 完整的提交号
	Time     time.Time `json:"time,omitzero"`      // 提交时间
	Modified bool      `json:"modified,omitempty"` // 构建时工作区有未提交的修改
}

var buildInfo = readBuildInfo()

func readBuildInfo() BuildInfo {
	b := BuildInfo{Version: version}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if b.Version == "" {
			b.Version = tagVersion(bi.Main.Version)
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				b.Commit = s.Value
			case "vcs.time":
				b.Time, _ = time.Parse(time.RFC3339, s.Value)
			case "vcs.modified":
				b.Modified = s.Value == "true"
			}
		}
	}
	if b.Version == "" && len(b.Commit) >= 7 {
		b.Version = b.Commit[:7]
	}
	return b
}

// String 用于启动日志。
func (b BuildInfo) String() string {
	if b.Version == "" {
		return "开发版"
	}
	if b.Modified {
		return b.Version + "（含未提交的修改）"
	}
	return b.Version
}
