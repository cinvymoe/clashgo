package cfg

import (
	"os"
	"path/filepath"
	"testing"
)

// repoRoot 定位到 clashctl 仓库根目录，真实配置从这里取。
func repoRoot(t *testing.T) string {
	t.Helper()
	root := os.Getenv("CLASHCTL_ROOT")
	if root == "" {
		root = "/data/project/clashctl"
	}
	if _, err := os.Stat(filepath.Join(root, "resources", "config.yaml")); err != nil {
		t.Skipf("跳过：找不到真实配置 %s", root)
	}
	return root
}

func loadPair(t *testing.T) (configPath, mixinPath string) {
	t.Helper()
	root := repoRoot(t)
	return filepath.Join(root, "resources", "config.yaml"), filepath.Join(root, "resources", "mixin.yaml")
}

// TestMergeSyntax 是最基本的守卫：真实配置合并后必须能解析、且关键字段齐全。
func TestMergeSyntax(t *testing.T) {
	configPath, mixinPath := loadPair(t)
	cfgData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	mixData, err := os.ReadFile(mixinPath)
	if err != nil {
		t.Fatal(err)
	}
	cfgNode, err := Parse(cfgData)
	if err != nil {
		t.Fatalf("解析 config: %v", err)
	}
	mixNode, err := Parse(mixData)
	if err != nil {
		t.Fatalf("解析 mixin: %v", err)
	}
	merged, err := Merge(cfgNode, mixNode)
	if err != nil {
		t.Fatalf("合并: %v", err)
	}
	out, err := Render(merged)
	if err != nil {
		t.Fatalf("渲染: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("渲染结果为空")
	}
	// 合并结果必须仍然可解析
	if _, err := Parse(out); err != nil {
		t.Fatalf("合并结果无法再解析: %v", err)
	}
	for _, key := range []string{"mixed-port", "mode", "proxies", "proxy-groups", "rules", "secret", "external-ui"} {
		if mapIndex(merged, key) < 0 {
			t.Errorf("合并结果缺少顶层 key %q", key)
		}
	}
	if mapIndex(merged, "_custom") >= 0 {
		t.Error("_custom 属于 clashctl 私有元数据，不应进入 runtime")
	}
}
