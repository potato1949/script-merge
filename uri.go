package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// parseURI 把一条分享链接转换为 Clash 节点，无法识别时返回 nil。
// 新增协议：在 switch 中加一个 case 并实现对应的 parseXxx。
func parseURI(line string) map[string]any {
	scheme, _, ok := strings.Cut(line, "://")
	if !ok {
		return nil
	}
	var p map[string]any
	switch strings.ToLower(scheme) {
	case "ss":
		p = parseSS(line)
	case "vmess":
		p = parseVmess(line)
	case "trojan":
		p = parseTrojan(line)
	case "vless":
		p = parseVless(line)
	case "hysteria2", "hy2":
		p = parseHysteria2(line)
	}
	if p == nil || p["server"] == "" || p["port"] == 0 {
		return nil
	}
	if name, _ := p["name"].(string); name == "" {
		p["name"] = fmt.Sprintf("%v:%v", p["server"], p["port"])
	}
	return p
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func isTrue(s string) bool { return s == "1" || strings.EqualFold(s, "true") }

func parseSS(line string) map[string]any {
	rest := line[len("ss://"):]
	name := ""
	if i := strings.Index(rest, "#"); i >= 0 {
		name, _ = url.PathUnescape(rest[i+1:])
		rest = rest[:i]
	}
	query := ""
	if i := strings.Index(rest, "?"); i >= 0 {
		query = rest[i+1:]
		rest = rest[:i]
	}
	rest = strings.TrimSuffix(rest, "/")
	var userinfo, hostport string
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		userinfo, hostport = rest[:i], rest[i+1:]
		if dec, ok := decodeBase64(userinfo); ok && strings.Contains(dec, ":") {
			userinfo = dec
		} else if u, err := url.PathUnescape(userinfo); err == nil {
			userinfo = u
		}
	} else {
		dec, ok := decodeBase64(rest)
		if !ok {
			return nil
		}
		i := strings.LastIndex(dec, "@")
		if i < 0 {
			return nil
		}
		userinfo, hostport = dec[:i], dec[i+1:]
	}
	method, password, ok := strings.Cut(userinfo, ":")
	if !ok {
		return nil
	}
	host, port := splitHostPort(hostport)
	p := map[string]any{
		"name": name, "type": "ss", "server": host, "port": port,
		"cipher": method, "password": password, "udp": true,
	}
	if q, err := url.ParseQuery(query); err == nil {
		if plugin := q.Get("plugin"); plugin != "" {
			parts := strings.Split(plugin, ";")
			opts := map[string]string{}
			for _, kv := range parts[1:] {
				k, v, _ := strings.Cut(kv, "=")
				opts[k] = v
			}
			switch parts[0] {
			case "obfs-local", "simple-obfs":
				p["plugin"] = "obfs"
				p["plugin-opts"] = map[string]any{"mode": opts["obfs"], "host": opts["obfs-host"]}
			case "v2ray-plugin":
				po := map[string]any{"mode": "websocket", "host": opts["host"], "path": opts["path"]}
				if _, tls := opts["tls"]; tls {
					po["tls"] = true
				}
				p["plugin"] = "v2ray-plugin"
				p["plugin-opts"] = po
			}
		}
	}
	return p
}

func splitHostPort(hp string) (string, int) {
	i := strings.LastIndex(hp, ":")
	if i < 0 {
		return hp, 0
	}
	return strings.Trim(hp[:i], "[]"), atoi(hp[i+1:])
}

func parseVmess(line string) map[string]any {
	dec, ok := decodeBase64(line[len("vmess://"):])
	if !ok {
		return nil
	}
	var raw map[string]any
	if json.Unmarshal([]byte(dec), &raw) != nil {
		return nil
	}
	get := func(k string) string {
		switch v := raw[k].(type) {
		case string:
			return v
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		case bool:
			return strconv.FormatBool(v)
		}
		return ""
	}
	cipher := get("scy")
	if cipher == "" {
		cipher = "auto"
	}
	p := map[string]any{
		"name": get("ps"), "type": "vmess", "server": get("add"), "port": atoi(get("port")),
		"uuid": get("id"), "alterId": atoi(get("aid")), "cipher": cipher, "udp": true,
	}
	if tls := get("tls"); tls != "" && tls != "none" {
		p["tls"] = true
		if sni := firstNonEmpty(get("sni"), get("host")); sni != "" {
			p["servername"] = sni
		}
	}
	applyTransport(p, get("net"), get("host"), get("path"), get("path"))
	return p
}

// applyTransport 设置 ws / grpc / h2 传输层参数。
func applyTransport(p map[string]any, network, host, path, serviceName string) {
	switch network {
	case "ws":
		p["network"] = "ws"
		opts := map[string]any{}
		if path != "" {
			opts["path"] = path
		}
		if host != "" {
			opts["headers"] = map[string]any{"Host": host}
		}
		p["ws-opts"] = opts
	case "grpc":
		p["network"] = "grpc"
		p["grpc-opts"] = map[string]any{"grpc-service-name": serviceName}
	case "h2", "http":
		p["network"] = "h2"
		opts := map[string]any{}
		if path != "" {
			opts["path"] = path
		}
		if host != "" {
			opts["host"] = []any{host}
		}
		p["h2-opts"] = opts
	}
}

func parseStd(line string) (*url.URL, url.Values, string) {
	u, err := url.Parse(line)
	if err != nil || u.Hostname() == "" {
		return nil, nil, ""
	}
	return u, u.Query(), u.Fragment
}

func parseTrojan(line string) map[string]any {
	u, q, name := parseStd(line)
	if u == nil {
		return nil
	}
	p := map[string]any{
		"name": name, "type": "trojan", "server": u.Hostname(), "port": atoi(u.Port()),
		"password": u.User.Username(), "udp": true,
	}
	if sni := firstNonEmpty(q.Get("sni"), q.Get("peer")); sni != "" {
		p["sni"] = sni
	}
	if isTrue(q.Get("allowInsecure")) || isTrue(q.Get("insecure")) {
		p["skip-cert-verify"] = true
	}
	applyTransport(p, q.Get("type"), q.Get("host"), q.Get("path"), q.Get("serviceName"))
	return p
}

func parseVless(line string) map[string]any {
	u, q, name := parseStd(line)
	if u == nil {
		return nil
	}
	p := map[string]any{
		"name": name, "type": "vless", "server": u.Hostname(), "port": atoi(u.Port()),
		"uuid": u.User.Username(), "udp": true,
	}
	if flow := q.Get("flow"); flow != "" {
		p["flow"] = flow
	}
	switch q.Get("security") {
	case "tls":
		p["tls"] = true
	case "reality":
		p["tls"] = true
		p["reality-opts"] = map[string]any{"public-key": q.Get("pbk"), "short-id": q.Get("sid")}
	}
	if sni := q.Get("sni"); sni != "" {
		p["servername"] = sni
	}
	if fp := q.Get("fp"); fp != "" {
		p["client-fingerprint"] = fp
	}
	if isTrue(q.Get("allowInsecure")) {
		p["skip-cert-verify"] = true
	}
	applyTransport(p, q.Get("type"), q.Get("host"), q.Get("path"), q.Get("serviceName"))
	return p
}

func parseHysteria2(line string) map[string]any {
	u, q, name := parseStd(line)
	if u == nil {
		return nil
	}
	password := u.User.Username()
	if pw, ok := u.User.Password(); ok {
		password += ":" + pw
	}
	port := atoi(u.Port())
	if port == 0 {
		port = 443
	}
	p := map[string]any{
		"name": name, "type": "hysteria2", "server": u.Hostname(), "port": port,
		"password": password,
	}
	if sni := q.Get("sni"); sni != "" {
		p["sni"] = sni
	}
	if isTrue(q.Get("insecure")) {
		p["skip-cert-verify"] = true
	}
	if obfs := q.Get("obfs"); obfs != "" && obfs != "none" {
		p["obfs"] = obfs
		p["obfs-password"] = q.Get("obfs-password")
	}
	return p
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
