package cfg

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Lookup 按路径逐层向下取子节点；任一层缺失返回 nil。
// 例如 Lookup(doc, "_custom", "system-proxy", "enable")。
func Lookup(doc *yaml.Node, path ...string) *yaml.Node {
	cur := rootNode(doc)
	for _, k := range path {
		if cur == nil {
			return nil
		}
		cur = mapGet(cur, k)
	}
	return cur
}

// SetPath 把 doc 里 path 对应的值设为 val，中间缺失的映射会自动创建。
// 与 mapSet 一样，新 key 追加在末尾，已有 key 保持原位置。
func SetPath(doc *yaml.Node, val *yaml.Node, path ...string) error {
	if len(path) == 0 {
		return fmt.Errorf("路径为空")
	}
	cur := rootNode(doc)
	if cur == nil {
		// 空文件：造一个空映射，并把它挂回文档
		cur = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
			doc.Content[0] = cur
		} else if doc.Kind == 0 {
			// 完全空白的文件，unmarshal 出来是零值 Node
			doc.Kind = yaml.DocumentNode
			doc.Tag = ""
			doc.Content = []*yaml.Node{cur}
		} else {
			doc.Content = []*yaml.Node{cur}
		}
	}
	if cur.Kind != yaml.MappingNode {
		return fmt.Errorf("顶层不是映射，无法设置 %v", path)
	}
	for _, k := range path[:len(path)-1] {
		next := mapGet(cur, k)
		if next == nil || next.Kind != yaml.MappingNode {
			// 缺失或类型不对：直接换成新映射
			next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			mapSet(cur, k, next)
		}
		cur = next
	}
	mapSet(cur, path[len(path)-1], val)
	return nil
}

// DeleteKey 从文档根映射里删除一个 key。
func DeleteKey(doc *yaml.Node, key string) {
	if root := rootNode(doc); root != nil {
		mapDel(root, key)
	}
}

// Scalar 取标量值，并报告它是否真的是标量。
func Scalar(n *yaml.Node) (string, bool) {
	if n == nil || n.Kind != yaml.ScalarNode {
		return "", false
	}
	return n.Value, true
}

// String 取标量字符串，非标量返回空串。
func String(n *yaml.Node) string {
	v, _ := Scalar(n)
	return v
}

// Bool 取布尔值，非布尔返回 def。
func Bool(n *yaml.Node, def bool) bool {
	v, ok := Scalar(n)
	if !ok {
		return def
	}
	return v == "true"
}

// Int 取整数值，解析失败返回 def。
func Int(n *yaml.Node, def int) int {
	v, ok := Scalar(n)
	if !ok {
		return def
	}
	var i int
	if _, err := fmt.Sscanf(v, "%d", &i); err != nil {
		return def
	}
	return i
}

// StringNode 造一个字符串标量节点。
func StringNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

// BoolNode 造一个布尔标量节点。
func BoolNode(v bool) *yaml.Node {
	s := "false"
	if v {
		s = "true"
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: s}
}

// IntNode 造一个整数标量节点。
func IntNode(v int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprintf("%d", v)}
}
