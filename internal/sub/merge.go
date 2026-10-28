package sub

import (
	"fmt"
	"os"

	"clashgo/internal/cfg"
	"clashgo/internal/env"
)

// MergeConfig 读 config.yaml + mixin.yaml，产出 runtime 配置字节。
// 这是 bash 版 `_merge_config()` 的 Go 版入口，供 sub / mixin / secret 共用。
func MergeConfig(e *env.Env) ([]byte, error) {
	base, err := os.ReadFile(e.ConfigBase())
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", e.ConfigBase(), err)
	}
	mixinData, err := os.ReadFile(e.ConfigMixin())
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", e.ConfigMixin(), err)
	}
	cfgNode, err := cfg.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", e.ConfigBase(), err)
	}
	mixNode, err := cfg.Parse(mixinData)
	if err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", e.ConfigMixin(), err)
	}
	merged, err := cfg.Merge(cfgNode, mixNode)
	if err != nil {
		return nil, err
	}
	return cfg.Render(merged)
}
