package cfg

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Merge 复刻 clashctl 里 `_merge_config()` 的 yq 表达式，输入是订阅配置和 mixin，
// 输出是喂给内核的 runtime 配置。
//
// 语义契约（必须与 bash 版逐字节一致，见 docs/design.md 第 4 节）：
//  1. mixin 的 `_custom` 段是 clashctl 私有元数据，不进入 runtime
//  2. 顶层先做 map 深合并，同 key 的 map 递归合并，标量/序列/跨类型整体由 mixin 覆盖
//  3. `rules` = mixin.rules.prefix + config.rules + mixin.rules.suffix，整体替换
//  4. `proxies` = prefix + (config.proxies 按 name 应用 override) + suffix
//  5. `proxy-groups` 同上
//  6. inject 按 group name 把 proxy 名追加到该 group 的 .proxies，并保序去重
func Merge(config, mixin *yaml.Node) (*yaml.Node, error) {
	cfgRoot := rootNode(config)
	mixRoot := rootNode(mixin)
	if cfgRoot == nil {
		// 空订阅按空映射处理，对齐 yq 表达式里的 `($config // {})`。
		// 这决定了「装完还没加订阅」时 clashgo on 能不能跑起来：
		// bash 版在空 config.yaml 上会得到「只有 mixin」的 runtime，Go 版必须一致。
		cfgRoot = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}
	if mixRoot == nil {
		// 没有 mixin 时，输出就是订阅配置本身（但列表项仍需按下方规则整形成序列）
		mixRoot = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}
	if cfgRoot.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("订阅配置顶层不是映射，无法合并")
	}
	if mixRoot.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("mixin 配置顶层不是映射，无法合并")
	}
	cfgRoot = cloneNode(cfgRoot)
	mixRoot = cloneNode(mixRoot)

	// 1. _custom 是 clashctl 私有的元数据区，不写进 runtime
	rmap := deepMerge(cloneNode(cfgRoot), mixRoot)
	mapDel(rmap, "_custom")

	// 3/4/5. 三组列表各自按 prefix + config + suffix 重建
	for _, key := range []string{"rules", "proxies", "proxy-groups"} {
		mapSet(rmap, key, newSeq(buildList(key, cfgRoot, mixRoot)))
	}

	// 6. inject：把额外节点追加进已有 group
	inject := mapGet(mixRoot, "proxy-groups")
	injectMap := asMap(mapGet(inject, "inject"))
	groups := asSeq(mapGet(rmap, "proxy-groups"))
	if groups != nil {
		for _, g := range groups.Content {
			if g.Kind != yaml.MappingNode {
				continue
			}
			cur := seqItems(mapGet(g, "proxies"))
			if injectMap != nil {
				if name := scalarValue(mapGet(g, "name")); name != "" {
					if extra := asSeq(mapGet(injectMap, name)); extra != nil {
						cur = append(cur, extra.Content...)
					}
				}
			}
			// 无论有没有 inject 命中，这一轮都会重建 .proxies 并去重，
			// 复刻 yq 里 `.proxies = (.proxies + $extra | unique)` 的副作用：
			// 原本没有 proxies 字段的 group 会得到 proxies: []，重复项被去掉。
			mapSet(g, "proxies", newSeq(uniqueNodes(cur)))
		}
	}

	return rmap, nil
}

// buildList 组装 rules / proxies / proxy-groups 这一组列表。
func buildList(key string, cfgRoot, mixRoot *yaml.Node) []*yaml.Node {
	mixinList := asMap(mapGet(mixRoot, key))
	out := make([]*yaml.Node, 0, 32)
	out = append(out, seqItems(mapGet(mixinList, "prefix"))...)
	out = append(out, applyOverride(cfgRoot, key, mixinList)...)
	out = append(out, seqItems(mapGet(mixinList, "suffix"))...)
	return out
}

// applyOverride 把 config 原有列表按 name 套用 mixin 的同名 override；
// 没有命中 override 的项原样保留。
func applyOverride(cfgRoot *yaml.Node, key string, mixinList *yaml.Node) []*yaml.Node {
	items := seqItems(mapGet(cfgRoot, key))
	overrides := seqItems(mapGet(mixinList, "override"))
	out := make([]*yaml.Node, 0, len(items))
	for _, it := range items {
		replaced := false
		if name := scalarValue(mapGet(it, "name")); name != "" {
			for _, o := range overrides {
				if o.Kind != yaml.MappingNode {
					continue
				}
				if scalarValue(mapGet(o, "name")) == name {
					out = append(out, cloneNode(o))
					replaced = true
					break
				}
			}
		}
		if !replaced {
			out = append(out, cloneNode(it))
		}
	}
	return out
}

// Render 把节点序列化成 YAML 文本，格式与 `yq '.'` 一致：2 空格缩进、保留注释、
// 以单个换行结尾。
func Render(n *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Parse 解析 YAML 文本，返回文档节点。
func Parse(data []byte) (*yaml.Node, error) {
	var n yaml.Node
	if err := yaml.Unmarshal(data, &n); err != nil {
		return nil, err
	}
	return &n, nil
}
