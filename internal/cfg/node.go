package cfg

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// rootNode 把文档节点展开成根节点；空文档返回 nil。
func rootNode(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	if n.Kind == 0 {
		// 空输入 unmarshal 出来是零值节点
		return nil
	}
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		return n.Content[0]
	}
	if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
		return nil
	}
	return n
}

// asMap 只在 n 确实是映射时返回自身，否则返回 nil。
func asMap(n *yaml.Node) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	return n
}

// asSeq 只在 n 确实是序列时返回自身，否则返回 nil。
func asSeq(n *yaml.Node) *yaml.Node {
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	return n
}

// mapIndex 在映射里查找 key，返回键节点下标；未找到返回 -1。
func mapIndex(m *yaml.Node, key string) int {
	if m == nil || m.Kind != yaml.MappingNode {
		return -1
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

// mapGet 取映射里某个 key 的值节点；不存在返回 nil。
func mapGet(m *yaml.Node, key string) *yaml.Node {
	if i := mapIndex(m, key); i >= 0 {
		return m.Content[i+1]
	}
	return nil
}

// mapSet 把映射里某个 key 的值替换成 val；key 不存在则追加到末尾。
// 追加位置在末尾，和 yq 的 `*` 一致：订阅原有的 key 保持原顺序，mixin 独有的 key 排在后面。
func mapSet(m *yaml.Node, key string, val *yaml.Node) {
	m.Kind = yaml.MappingNode
	m.Tag = "!!map"
	if i := mapIndex(m, key); i >= 0 {
		m.Content[i+1] = val
		return
	}
	m.Content = append(m.Content, &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!str",
		Value: key,
	}, val)
}

// mapDel 删除映射里的某个 key。
func mapDel(m *yaml.Node, key string) {
	i := mapIndex(m, key)
	if i < 0 {
		return
	}
	m.Content = append(m.Content[:i], m.Content[i+2:]...)
}

// cloneNode 深拷贝一个节点，用于避免两个输入文档互相污染。
func cloneNode(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	c := *n
	if n.Content != nil {
		c.Content = make([]*yaml.Node, len(n.Content))
		for i, child := range n.Content {
			c.Content[i] = cloneNode(child)
		}
	}
	if n.Alias != nil {
		c.Alias = cloneNode(n.Alias)
	}
	return &c
}

// seqItems 把值节点当作序列取出元素；nil / null / 非序列一律当作空。
func seqItems(n *yaml.Node) []*yaml.Node {
	if s := asSeq(n); s != nil {
		return s.Content
	}
	return nil
}

// newSeq 造一个块状序列节点，元素为 items 的深拷贝。
func newSeq(items []*yaml.Node) *yaml.Node {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	seq.Content = make([]*yaml.Node, 0, len(items))
	for _, it := range items {
		seq.Content = append(seq.Content, cloneNode(it))
	}
	return seq
}

// nodeKey 给节点算一个用于去重的稳定标识（依赖 yaml 序列化结果）。
func nodeKey(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	if n.Kind == yaml.ScalarNode {
		return "s:" + n.Tag + ":" + n.Value
	}
	b, err := yaml.Marshal(n)
	if err != nil {
		return fmt.Sprintf("?%p", n)
	}
	return "n:" + string(b)
}

// uniqueNodes 保序去重，语义与 jq/yq 的 `unique` 一致。
func uniqueNodes(items []*yaml.Node) []*yaml.Node {
	seen := make(map[string]struct{}, len(items))
	out := make([]*yaml.Node, 0, len(items))
	for _, it := range items {
		k := nodeKey(it)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, it)
	}
	return out
}

// scalarValue 取标量值，非标量返回空串。
func scalarValue(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

// mergeComments 把 src 键节点上的注释搬到 dst 上：src 有注释就覆盖，没有就保留 dst 原有的。
// 这是实测出来的 yq 行为，被 mixin 覆盖的 key 以前者注释为准，但 mixin 没写注释时不会把原有注释抹掉。
func mergeComments(dst, src *yaml.Node) {
	if src == nil {
		return
	}
	if src.HeadComment != "" {
		dst.HeadComment = src.HeadComment
	}
	if src.LineComment != "" {
		dst.LineComment = src.LineComment
	}
	if src.FootComment != "" {
		dst.FootComment = src.FootComment
	}
}

// preferredStyle 决定替换后的值节点用哪种排版风格。
// 实测 yq 的规则是「dst 有显式风格就用 dst，否则用 src」：
// 例如 dst 是 flow 序列时，即使 mixin 用块状写法，输出仍是 flow。
func preferredStyle(dst, src *yaml.Node) yaml.Style {
	if dst != nil && dst.Style != 0 {
		return dst.Style
	}
	if src != nil {
		return src.Style
	}
	return 0
}

// deepMerge 复刻 yq 的 `$config * $mixin`：
//   - 映射与映射递归合并
//   - 其余情况（标量、序列、类型不同、null）由 src 整体替换 dst
//
// 替换时内容取自 src，但排版风格优先沿用 dst，这与 yq 的输出逐字节一致。
func deepMerge(dst, src *yaml.Node) *yaml.Node {
	if asMap(src) == nil {
		return mergeReplacement(dst, src)
	}
	if asMap(dst) == nil {
		return cloneNode(src)
	}
	for i := 0; i+1 < len(src.Content); i += 2 {
		sk, sv := src.Content[i], src.Content[i+1]
		if pos := mapIndex(dst, sk.Value); pos >= 0 {
			dk := dst.Content[pos]
			mergeComments(dk, sk)
			if asMap(sv) != nil && asMap(dst.Content[pos+1]) != nil {
				dst.Content[pos+1] = deepMerge(dst.Content[pos+1], sv)
				continue
			}
			dst.Content[pos+1] = mergeReplacement(dst.Content[pos+1], sv)
			continue
		}
		dst.Content = append(dst.Content, cloneNode(sk), cloneNode(sv))
	}
	return dst
}

// mergeReplacement 用 src 的值替换 dst 的值：内容来自 src，风格沿用 dst 的显式风格。
func mergeReplacement(dst, src *yaml.Node) *yaml.Node {
	nv := cloneNode(src)
	if nv == nil {
		return nil
	}
	nv.Style = preferredStyle(dst, src)
	if dst != nil {
		if nv.LineComment == "" {
			nv.LineComment = dst.LineComment
		}
		if nv.HeadComment == "" {
			nv.HeadComment = dst.HeadComment
		}
		if nv.FootComment == "" {
			nv.FootComment = dst.FootComment
		}
	}
	return nv
}
