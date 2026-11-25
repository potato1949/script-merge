package main

import (
	"fmt"
	"strings"
)

// 订阅组追加的策略组（Profile.Groups）。底板的策略组和规则不动，只在 proxy-groups 末尾追加；
// 在 Parser 之前执行，所以 Parser 可以引用这些组（比如把它加进底板的某个组）。

// GroupStatus 是一个追加的策略组在本次构建里的结果，给界面显示。
type GroupStatus struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Members []string `json:"members"`           // 成员：底板策略组在前，节点在后。没有成员时是 [DIRECT]
	Groups  int      `json:"groups"`            // Members 里底板策略组的个数
	Skipped string   `json:"skipped,omitempty"` // 没有生成的原因
}

// addGroups 把 defs 追加到 groups（底板改写后的策略组）末尾，nodes 是输出里全部节点的名字（按输出顺序）。
// 选中的底板策略组已经不存在（机场改了名）时跳过这个成员；和已有的策略组、节点重名的整组跳过；
// 一个成员都没有时填 DIRECT。这些都只给警告。
func addGroups(defs []ProxyGroup, groups []any, nodes []string) (out []any, stats []GroupStatus, warns []string) {
	base, taken := map[string]bool{}, map[string]bool{}
	for _, n := range namesOf(groups) {
		base[n], taken[n] = true, true
	}
	for _, n := range nodes {
		taken[n] = true
	}
	for _, d := range defs {
		st := GroupStatus{ID: d.ID, Name: d.Name, Members: []string{}}
		switch {
		case base[d.Name]:
			st.Skipped = "和底板的策略组重名"
		case taken[d.Name]:
			st.Skipped = "和节点重名"
		}
		if st.Skipped != "" {
			warns = append(warns, fmt.Sprintf("新增的策略组「%s」%s，没有生成", d.Name, st.Skipped))
			stats = append(stats, st)
			continue
		}
		taken[d.Name] = true
		for _, g := range d.Groups {
			if !base[g] {
				warns = append(warns, fmt.Sprintf("新增的策略组「%s」选用的底板策略组「%s」不存在，已跳过", d.Name, g))
				continue
			}
			st.Members = append(st.Members, g)
		}
		st.Groups = len(st.Members)
		re, err := optRegex(d.Filter)
		if err != nil {
			warns = append(warns, fmt.Sprintf("新增的策略组「%s」的节点%v", d.Name, err))
		}
		if re != nil {
			for _, n := range nodes {
				if reMatch(re, n) {
					st.Members = append(st.Members, n)
				}
			}
		}
		if len(st.Members) == 0 {
			st.Members = []string{"DIRECT"}
			warns = append(warns, fmt.Sprintf("新增的策略组「%s」没有成员，已填入 DIRECT", d.Name))
		}
		members := make([]any, len(st.Members))
		for i, m := range st.Members {
			members[i] = m
		}
		groups = append(groups, map[string]any{"name": d.Name, "type": "select", "proxies": members})
		stats = append(stats, st)
	}
	return groups, stats, warns
}

// normalizeGroups 规范化订阅组追加的策略组：ID 格式不对或重复的重新生成，组名去掉首尾空白且不能为空、不能重复、
// 不能是 DIRECT 这类内置策略，选用的底板策略组去掉空的和重复的。
func normalizeGroups(p *Profile) error {
	ids, names := map[string]bool{}, map[string]bool{}
	if p.Groups == nil {
		p.Groups = []ProxyGroup{}
	}
	for i := range p.Groups {
		g := &p.Groups[i]
		if !validID(g.ID) || ids[g.ID] {
			g.ID = randHex(8)
		}
		ids[g.ID] = true
		g.Name = strings.TrimSpace(g.Name)
		switch {
		case g.Name == "":
			return fmt.Errorf("订阅组「%s」的第 %d 个新增策略组没有填名称", p.Name, i+1)
		case builtinPolicies[g.Name]:
			return fmt.Errorf("订阅组「%s」的新增策略组不能叫「%s」", p.Name, g.Name)
		case names[g.Name]:
			return fmt.Errorf("订阅组「%s」里有重名的新增策略组「%s」", p.Name, g.Name)
		}
		names[g.Name] = true
		seen, list := map[string]bool{}, []string{}
		for _, n := range g.Groups {
			if n = strings.TrimSpace(n); n != "" && !seen[n] {
				seen[n] = true
				list = append(list, n)
			}
		}
		g.Groups = list
	}
	return nil
}
