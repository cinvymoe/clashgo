// Package mixin 负责读写 resources/mixin.yaml。
//
// mixin 是「用户手写的覆盖层」：订阅每次更新都会覆盖 config.yaml，而 mixin
// 不会被动到，所以用户的个性化配置（端口、密钥、TUN、规则前后缀）都住在这里。
//
// 这个包的关键约束是**保留格式**：mixin 里有大量注释（包括被注释掉的
// `#authentication:` 和 inject 示例），编辑后必须原样保留，否则用户的自定义
// 说明文档就丢了。所以全部操作都在 yaml.Node 上做，并用 Encoder SetIndent(2)。
package mixin

import (
	"fmt"
	"os"
	"path/filepath"

	"clashgo/internal/cfg"
	"clashgo/internal/env"
	"gopkg.in/yaml.v3"
)

// Doc 是一份已解析的 mixin 文档。
type Doc struct {
	Env  *env.Env
	Node *yaml.Node
	raw  []byte
}

// Load 读取并解析 mixin.yaml。文件不存在时返回空文档（不算错误：
// 首次使用还没写过 mixin 是正常状态）。
func Load(e *env.Env) (*Doc, error) {
	path := e.ConfigMixin()
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("读取 %s 失败: %w", path, err)
		}
		raw = nil
	}
	node, err := cfg.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	return &Doc{Env: e, Node: node, raw: raw}, nil
}

// Get 按路径取值节点，不存在返回 nil。
func (d *Doc) Get(path ...string) *yaml.Node {
	return cfg.Lookup(d.Node, path...)
}

// String 取某个路径的字符串值。
func (d *Doc) String(path ...string) string {
	return cfg.String(d.Get(path...))
}

// Bool 取某个路径的布尔值。
func (d *Doc) Bool(def bool, path ...string) bool {
	return cfg.Bool(d.Get(path...), def)
}

// Int 取某个路径的整数值。
func (d *Doc) Int(def int, path ...string) int {
	return cfg.Int(d.Get(path...), def)
}

// SetString 设置字符串值。
func (d *Doc) SetString(v string, path ...string) error {
	return cfg.SetPath(d.Node, cfg.StringNode(v), path...)
}

// SetBool 设置布尔值。
func (d *Doc) SetBool(v bool, path ...string) error {
	return cfg.SetPath(d.Node, cfg.BoolNode(v), path...)
}

// SetInt 设置整数值。
func (d *Doc) SetInt(v int, path ...string) error {
	return cfg.SetPath(d.Node, cfg.IntNode(v), path...)
}

// Delete 删除某个路径的键（不存在时无操作）。
func (d *Doc) Delete(key string) {
	if root := d.Node; root != nil {
		cfg.DeleteKey(root, key)
	}
}

// Render 序列化成 YAML 文本（保留注释与顺序）。
func (d *Doc) Render() ([]byte, error) {
	if d.Node == nil {
		return nil, nil
	}
	return cfg.Render(d.Node)
}

// Save 原子写回 mixin.yaml。内容没变化时不落盘，避免无意义的文件时间戳抖动。
func (d *Doc) Save() (changed bool, err error) {
	out, err := d.Render()
	if err != nil {
		return false, err
	}
	if string(out) == string(d.raw) {
		return false, nil
	}
	path := d.Env.ConfigMixin()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("创建目录失败: %w", err)
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return false, fmt.Errorf("写入 %s 失败: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false, fmt.Errorf("替换 %s 失败: %w", path, err)
	}
	d.raw = out
	return true, nil
}

// Raw 返回最近一次读取/保存后的原始字节。
func (d *Doc) Raw() []byte { return d.raw }
