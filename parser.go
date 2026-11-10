package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/dlclark/regexp2"
	"gopkg.in/yaml.v3"
)

// 本文件实现 CFW Parsers 的 YAML 语法（不支持 JavaScript 形式）：
//   prepend-<key> / append-<key>  数组合并到 <key> 前/后
//   mix-<key>                     对象合并到 <key>；mix-object 合并到最外层
//   commands                      路径命令，在以上操作全部完成后按顺序执行
//
// commands 语法：<定位><操作><设定值>
//   定位以 . 分隔，数字为数组下标，(名称) 或直接写名称按 name 定位数组元素
//   =  覆盖    +  插入（有下标则插到该位置，否则追加）    结尾的 -  删除该定位
//   _  按值删除（Clash-Parser-Online 扩展，例：proxy-groups.(美国).proxies_节点名）
//   设定值可为 JSON，或 []proxyNames|正则、[]groupNames|正则、[]shuffledProxyNames|正则

func compileRegex(pattern string) (*regexp2.Regexp, error) {
	re, err := regexp2.Compile(pattern, regexp2.None)
	if err != nil {
		return nil, fmt.Errorf("正则 %q 无效: %w", pattern, err)
	}
	re.MatchTimeout = 2 * time.Second
	return re, nil
}

// optRegex 编译可为空的正则；空串返回 nil。
func optRegex(pattern string) (*regexp2.Regexp, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, nil
	}
	return compileRegex(pattern)
}

func reMatch(re *regexp2.Regexp, s string) bool {
	ok, _ := re.MatchString(s)
	return ok
}

// ApplyParser 把 parser 文本应用到 root 上，返回被跳过的命令等警告。
func ApplyParser(root map[string]any, text string) ([]string, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("Parser YAML 解析失败: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	var warns []string
	for _, body := range parserBodies(doc.Content[0]) {
		warns = append(warns, applyParserBody(root, body)...)
	}
	return warns, nil
}

// parserBodies 兼容三种写法：直接写指令、`yaml:` 包裹、CFW 原版的 `parsers: [{yaml: ...}]`。
func parserBodies(n *yaml.Node) []*yaml.Node {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	if y := mapValue(n, "yaml"); y != nil {
		return []*yaml.Node{y}
	}
	if ps := mapValue(n, "parsers"); ps != nil && ps.Kind == yaml.SequenceNode {
		var out []*yaml.Node
		for _, item := range ps.Content {
			if y := mapValue(item, "yaml"); y != nil {
				out = append(out, y)
			}
		}
		return out
	}
	return []*yaml.Node{n}
}

func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func applyParserBody(root map[string]any, body *yaml.Node) (warns []string) {
	if body.Kind != yaml.MappingNode {
		return []string{"Parser: 内容应为对象"}
	}
	var commands []string
	for i := 0; i+1 < len(body.Content); i += 2 {
		key := body.Content[i].Value
		var val any
		if err := body.Content[i+1].Decode(&val); err != nil {
			warns = append(warns, fmt.Sprintf("Parser: %s 解析失败: %v", key, err))
			continue
		}
		switch {
		case key == "commands":
			list, _ := val.([]any)
			for _, c := range list {
				commands = append(commands, fmt.Sprint(c))
			}
		case strings.HasPrefix(key, "prepend-"), strings.HasPrefix(key, "append-"):
			list, ok := val.([]any)
			if !ok {
				if val != nil {
					warns = append(warns, fmt.Sprintf("Parser: %s 应为数组", key))
				}
				continue
			}
			mode, target, _ := strings.Cut(key, "-")
			cur, _ := root[target].([]any)
			if mode == "prepend" {
				root[target] = append(append([]any{}, list...), cur...)
			} else {
				root[target] = append(cur, list...)
			}
		case strings.HasPrefix(key, "mix-"):
			obj, ok := val.(map[string]any)
			if !ok {
				if val != nil {
					warns = append(warns, fmt.Sprintf("Parser: %s 应为对象", key))
				}
				continue
			}
			dst := root
			if target := strings.TrimPrefix(key, "mix-"); target != "object" {
				m, _ := root[target].(map[string]any)
				if m == nil {
					m = map[string]any{}
					root[target] = m
				}
				dst = m
			}
			for k, v := range obj {
				dst[k] = v
			}
		default:
			warns = append(warns, fmt.Sprintf("Parser: 忽略未知指令 %s", key))
		}
	}
	for _, c := range commands {
		if err := runCommand(root, c); err != nil {
			warns = append(warns, fmt.Sprintf("Parser 命令 %q 已跳过: %v", c, err))
		}
	}
	return warns
}

type pathSeg struct {
	text   string
	byName bool // (名称) 形式
}

// parenEnd 返回从 start（指向 '('）开始的括号段的结束位置（')' 的下标），找不到返回 -1。
func parenEnd(s string, start int) int {
	for i := start + 1; i < len(s); i++ {
		if s[i] == ')' && (i+1 == len(s) || strings.IndexByte(".=+-_", s[i+1]) >= 0) {
			return i
		}
	}
	return -1
}

// findOp 在括号段之外寻找第一个属于 ops 的字符。
func findOp(cmd, ops string) int {
	segStart := true
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if segStart && c == '(' {
			if end := parenEnd(cmd, i); end > 0 {
				i = end
				segStart = false
				continue
			}
		}
		if strings.IndexByte(ops, c) >= 0 {
			return i
		}
		segStart = c == '.'
	}
	return -1
}

func parseCommand(cmd string) (segs []pathSeg, op byte, val string, err error) {
	cmd = strings.TrimSpace(cmd)
	var path string
	if i := findOp(cmd, "=+"); i >= 0 {
		path, op, val = cmd[:i], cmd[i], cmd[i+1:]
	} else if strings.HasSuffix(cmd, "-") {
		path, op = cmd[:len(cmd)-1], '-'
	} else if i := findOp(cmd, "_"); i >= 0 {
		path, op, val = cmd[:i], '_', cmd[i+1:]
	} else {
		return nil, 0, "", errors.New("缺少操作符 (= + - _)")
	}
	for i := 0; i < len(path); {
		if path[i] == '(' {
			if end := parenEnd(path, i); end > 0 {
				segs = append(segs, pathSeg{text: path[i+1 : end], byName: true})
				i = end + 1
				if i < len(path) && path[i] == '.' {
					i++
				}
				continue
			}
		}
		j := strings.IndexByte(path[i:], '.')
		if j < 0 {
			j = len(path) - i
		}
		segs = append(segs, pathSeg{text: path[i : i+j]})
		i += j + 1
	}
	if len(segs) == 0 || segs[0].text == "" {
		return nil, 0, "", errors.New("定位为空")
	}
	return segs, op, val, nil
}

func runCommand(root map[string]any, cmd string) error {
	segs, op, raw, err := parseCommand(cmd)
	if err != nil {
		return err
	}
	var val any = raw
	if op != '-' {
		if val, err = resolveValue(root, raw, op); err != nil {
			return err
		}
	}
	_, err = applyPath(root, segs, op, val)
	return err
}

func itemName(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case map[string]any:
		name, ok := t["name"].(string)
		return name, ok
	}
	return "", false
}

func namesOf(list any) []string {
	items, _ := list.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		if name, ok := itemName(it); ok {
			out = append(out, name)
		}
	}
	return out
}

func resolveValue(root map[string]any, raw string, op byte) (any, error) {
	if strings.HasPrefix(raw, "[]") {
		kind, pattern, _ := strings.Cut(raw[2:], "|")
		var names []string
		switch kind {
		case "proxyNames", "shuffledProxyNames":
			names = namesOf(root["proxies"])
		case "groupNames":
			names = namesOf(root["proxy-groups"])
		default:
			return nil, fmt.Errorf("未知的内置值 []%s", kind)
		}
		if pattern != "" {
			re, err := compileRegex(pattern)
			if err != nil {
				return nil, err
			}
			kept := names[:0]
			for _, n := range names {
				if reMatch(re, n) {
					kept = append(kept, n)
				}
			}
			names = kept
		}
		if kind == "shuffledProxyNames" {
			rand.Shuffle(len(names), func(i, j int) { names[i], names[j] = names[j], names[i] })
		}
		out := make([]any, len(names))
		for i, n := range names {
			out[i] = n
		}
		return out, nil
	}
	if op == '_' {
		return raw, nil // 按值删除时保留原始文本
	}
	var v any
	if json.Unmarshal([]byte(raw), &v) == nil {
		return normalizeNumbers(v), nil
	}
	return raw, nil
}

// normalizeNumbers 把 JSON 解码出的整数值 float64 还原为 int。
func normalizeNumbers(v any) any {
	switch t := v.(type) {
	case float64:
		if t == float64(int64(t)) {
			return int(t)
		}
	case []any:
		for i := range t {
			t[i] = normalizeNumbers(t[i])
		}
	case map[string]any:
		for k := range t {
			t[k] = normalizeNumbers(t[k])
		}
	}
	return v
}

func asList(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return []any{v}
}

func insertAt(list []any, idx int, vals []any) []any {
	out := make([]any, 0, len(list)+len(vals))
	out = append(out, list[:idx]...)
	out = append(out, vals...)
	return append(out, list[idx:]...)
}

// listIndex 把路径段解析为数组下标；找不到返回 -1。
func listIndex(list []any, s pathSeg) int {
	if !s.byName {
		if n, err := strconv.Atoi(s.text); err == nil {
			return n
		}
	}
	for i, it := range list {
		if name, ok := itemName(it); ok && name == s.text {
			return i
		}
	}
	return -1
}

func removeByValue(list []any, val any) []any {
	if raw, ok := val.(string); ok {
		if n, err := strconv.Atoi(raw); err == nil { // 纯数字视为下标
			if n >= 0 && n < len(list) {
				return append(list[:n:n], list[n+1:]...)
			}
			return list
		}
		if len(raw) >= 2 && raw[0] == '(' && raw[len(raw)-1] == ')' {
			raw = raw[1 : len(raw)-1]
		}
		val = []any{raw}
	}
	drop := map[string]bool{}
	for _, v := range asList(val) {
		drop[fmt.Sprint(v)] = true
	}
	out := []any{}
	for _, it := range list {
		if name, ok := itemName(it); ok && drop[name] {
			continue
		}
		out = append(out, it)
	}
	return out
}

// applyPath 沿定位递归执行操作，返回修改后的当前节点。
func applyPath(cur any, segs []pathSeg, op byte, val any) (any, error) {
	if len(segs) == 0 {
		switch op {
		case '=':
			return val, nil
		case '+':
			if cur == nil {
				return asList(val), nil
			}
			list, ok := cur.([]any)
			if !ok {
				return nil, errors.New("+ 的目标不是数组")
			}
			return append(list, asList(val)...), nil
		case '_':
			list, ok := cur.([]any)
			if !ok {
				return nil, errors.New("_ 的目标不是数组")
			}
			return removeByValue(list, val), nil
		}
		return nil, errors.New("无效操作")
	}
	s, last := segs[0], len(segs) == 1
	switch c := cur.(type) {
	case nil:
		if s.byName || op == '-' || op == '_' {
			return nil, fmt.Errorf("定位 %q 不存在", s.text)
		}
		return applyPath(map[string]any{}, segs, op, val)
	case map[string]any:
		if last && op == '-' {
			delete(c, s.text)
			return c, nil
		}
		child, err := applyPath(c[s.text], segs[1:], op, val)
		if err != nil {
			return nil, err
		}
		c[s.text] = child
		return c, nil
	case []any:
		idx := listIndex(c, s)
		if last && op != '_' {
			if idx < 0 || idx > len(c) || (idx == len(c) && op == '-') {
				return nil, fmt.Errorf("找不到 %q", s.text)
			}
			switch op {
			case '+':
				return insertAt(c, idx, asList(val)), nil
			case '-':
				return append(c[:idx:idx], c[idx+1:]...), nil
			default: // '='
				if idx == len(c) {
					return append(c, val), nil
				}
				c[idx] = val
				return c, nil
			}
		}
		if idx < 0 || idx >= len(c) {
			return nil, fmt.Errorf("找不到 %q", s.text)
		}
		child, err := applyPath(c[idx], segs[1:], op, val)
		if err != nil {
			return nil, err
		}
		c[idx] = child
		return c, nil
	}
	return nil, fmt.Errorf("定位 %q 处不是对象或数组", s.text)
}
