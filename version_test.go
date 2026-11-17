package main

import (
	"net/http"
	"testing"
)

// 程序版本只给已登录的人看；编译时指定的版本号优先于 git 提交。
func TestBuildInfo(t *testing.T) {
	_, do := newTestApp(t)
	status := func(edit ...func(*http.Request)) map[string]any {
		t.Helper()
		return decode[map[string]any](t, do("GET", "/api/status", "", edit...))
	}
	if _, ok := status()["build"]; !ok {
		t.Error("登录后的状态里应有程序版本")
	}
	if st := status(func(r *http.Request) { r.Header.Del("Cookie") }); st["authed"] != false || st["build"] != nil {
		t.Errorf("未登录时不应返回程序版本: %v", st)
	}

	defer func(v string) { version = v }(version)
	version = "v1.2.0"
	if b := readBuildInfo(); b.Version != "v1.2.0" || b.String() == "开发版" {
		t.Errorf("指定的版本号: %+v", b)
	}
	version = ""
	if b := readBuildInfo(); b.Version == "" && b.String() != "开发版" {
		t.Errorf("没有版本信息时: %q", b.String())
	}
}
