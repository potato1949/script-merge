package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestNodeHosts(t *testing.T) {
	up := &upstream{Doc: map[string]any{
		"hosts": map[string]any{
			"n1.node.sbs":  "n2.node.sbs", // 两层映射
			"n2.node.sbs":  []any{"1.2.3.4"},
			"+.other.sbs":  "x.sbs", // 通配：节点地址 a.other.sbs 匹配
			"unrelated.io": "5.6.7.8",
		},
		"dns": map[string]any{"fake-ip-filter": []any{"*.lan", "n1.node.sbs", "n2.node.sbs", "+.other.sbs", "geosite:cn"}},
	}}
	hosts, filter := nodeHosts(up, []string{"N1.node.sbs", "a.other.sbs", "9.9.9.9"})
	if want := map[string]any{"n1.node.sbs": "n2.node.sbs", "n2.node.sbs": []any{"1.2.3.4"}, "+.other.sbs": "x.sbs"}; !reflect.DeepEqual(hosts, want) {
		t.Errorf("hosts = %v", hosts)
	}
	if want := []string{"n1.node.sbs", "n2.node.sbs", "+.other.sbs"}; !reflect.DeepEqual(filter, want) {
		t.Errorf("fake-ip-filter = %v", filter)
	}

	root := map[string]any{"hosts": map[string]any{"n1.node.sbs": "别的"}, "dns": map[string]any{"fake-ip-filter": []any{"*.lan"}}}
	warns := mergeNodeHosts(root, hosts, filter)
	if len(warns) != 1 || root["hosts"].(map[string]any)["n1.node.sbs"] != "别的" || root["hosts"].(map[string]any)["n2.node.sbs"] == nil {
		t.Errorf("底板里已有的 hosts 应当保留并提醒: %v %v", root["hosts"], warns)
	}
	if got := root["dns"].(map[string]any)["fake-ip-filter"]; !reflect.DeepEqual(got, []any{"*.lan", "n1.node.sbs", "n2.node.sbs", "+.other.sbs"}) {
		t.Errorf("fake-ip-filter 合并结果 = %v", got)
	}
	white := map[string]any{"dns": map[string]any{"fake-ip-filter-mode": "whitelist"}}
	mergeNodeHosts(white, nil, filter)
	if white["dns"].(map[string]any)["fake-ip-filter"] != nil {
		t.Errorf("白名单模式下不应追加")
	}
}

// 上游配置里节点以外的内容整份留下；键不是字符串的对象也要能存进 JSON 缓存。
func TestParseSubscriptionKeepsDoc(t *testing.T) {
	up, err := parseSubscription([]byte(`port: 7890
proxies:
  - {name: a, type: http, server: 1.1.1.1, port: 80}
proxy-groups:
  - {name: G, type: select, proxies: [a]}
x-anchors:
  80: http
  true: yes
`))
	if err != nil {
		t.Fatal(err)
	}
	if up.Doc["proxies"] != nil || up.Doc["port"] != 7890 || len(up.groups()) != 1 {
		t.Errorf("Doc = %v", up.Doc)
	}
	if _, err := json.Marshal(up); err != nil {
		t.Errorf("不能存进缓存: %v", err)
	}
	if m, _ := up.Doc["x-anchors"].(map[string]any); m["80"] != "http" {
		t.Errorf("非字符串的键应转成字符串: %#v", up.Doc["x-anchors"])
	}
}
