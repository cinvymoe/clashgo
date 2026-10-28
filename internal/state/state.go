// Package state 负责读写 profiles.yaml（订阅清单）。
//
// 设计要点：清单里只保存 id 和 url 这两个真相，path 是派生值。
// bash 版把绝对路径写进文件，安装目录一搬家就失效；这里读取时按 id 重新派生，
// 只有写回时才把 path 写上（保持文件对其它工具的可读性）。
package state

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Profile 是一条订阅。
type Profile struct {
	ID   int    `yaml:"id"`
	Path string `yaml:"path,omitempty"`
	URL  string `yaml:"url"`
}

// Store 是 profiles.yaml 的内存表示。字段顺序刻意与 bash 版写出的文件一致
// （use 在前，profiles 在后），避免每次写回都产生无意义的 diff。
type Store struct {
	Use      int       `yaml:"use,omitempty"`
	Profiles []Profile `yaml:"profiles"`
}

// Path 是 profiles.yaml 的位置，以及 id → profile 文件路径的派生函数。
type Path struct {
	Meta     string
	ProfileF func(id int) string
}

// Load 读取订阅清单。文件不存在时返回空 Store 而不是错误——
// 首次使用时本来就没有清单，这不算异常。
func Load(p Path) (*Store, error) {
	data, err := os.ReadFile(p.Meta)
	if err != nil {
		if os.IsNotExist(err) {
			return &Store{}, nil
		}
		return nil, fmt.Errorf("读取 %s 失败: %w", p.Meta, err)
	}
	var s Store
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", p.Meta, err)
	}
	// path 一律重新派生，不信文件里存的绝对路径
	for i := range s.Profiles {
		s.Profiles[i].Path = p.ProfileF(s.Profiles[i].ID)
	}
	return &s, nil
}

// Save 原子写回订阅清单。
//
// 用 yaml.Node 手工搭树而不是直接 Marshal 结构体，是为了复刻 bash 版写出的
// 头注释与字段顺序（use 在前、profiles 在后），让两个 CLI 写出的文件完全一致。
func (s *Store) Save(p Path) error {
	root := &yaml.Node{Kind: yaml.MappingNode}
	if s.Use > 0 {
		key := keyNode("use")
		key.HeadComment = "当前使用的订阅"
		root.Content = append(root.Content, key, intNode(s.Use))
	}
	profKey := keyNode("profiles")
	profKey.HeadComment = "订阅列表"
	root.Content = append(root.Content, profKey, profilesNode(s.Profiles, p.ProfileF))

	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("序列化订阅清单失败: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("序列化订阅清单失败: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(p.Meta), 0o755); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}
	tmp := p.Meta + ".new"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", tmp, err)
	}
	if err := os.Rename(tmp, p.Meta); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换 %s 失败: %w", p.Meta, err)
	}
	return nil
}

func keyNode(k string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}
}

func intNode(v int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(v)}
}

func strNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

func profilesNode(list []Profile, profileF func(int) string) *yaml.Node {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, item := range list {
		m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		m.Content = append(m.Content,
			keyNode("id"), intNode(item.ID),
			keyNode("path"), strNode(profileF(item.ID)),
			keyNode("url"), strNode(item.URL),
		)
		seq.Content = append(seq.Content, m)
	}
	return seq
}

// Find 按 id 查找订阅，找不到返回 nil。
func (s *Store) Find(id int) *Profile {
	for i := range s.Profiles {
		if s.Profiles[i].ID == id {
			return &s.Profiles[i]
		}
	}
	return nil
}

// NextID 分配新订阅的 id：现有最大值 +1，空清单从 1 开始。
// 与 bash 版 `.profiles // [] | (map(.id) | max) // 0 | . + 1` 等价。
func (s *Store) NextID() int {
	max := 0
	for _, p := range s.Profiles {
		if p.ID > max {
			max = p.ID
		}
	}
	return max + 1
}

// Add 追加一条订阅。url 已存在时报错（bash 版同样拒绝重复链接）。
func (s *Store) Add(url string) (Profile, error) {
	for _, p := range s.Profiles {
		if p.URL == url {
			return Profile{}, fmt.Errorf("该订阅链接已存在（id=%d）", p.ID)
		}
	}
	p := Profile{ID: s.NextID(), URL: url}
	s.Profiles = append(s.Profiles, p)
	return p, nil
}

// Remove 删除订阅。正在使用中的订阅不允许删除，这是 bash 版的既有保护。
func (s *Store) Remove(id int) (Profile, error) {
	p := s.Find(id)
	if p == nil {
		return Profile{}, fmt.Errorf("订阅 id %d 不存在", id)
	}
	if s.Use == id {
		return Profile{}, fmt.Errorf("删除失败：订阅 %d 正在使用中，请先切换订阅", id)
	}
	removed := *p // 先拷贝：原地过滤会覆盖 p 指向的元素
	out := s.Profiles[:0]
	for _, item := range s.Profiles {
		if item.ID != id {
			out = append(out, item)
		}
	}
	s.Profiles = out
	return removed, nil
}
