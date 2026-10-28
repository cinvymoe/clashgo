package cfg

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestMergeCases 用一组小的合成配置锁定合并语义的每个角落。
// 每个 case 目录下放 config.yaml + mixin.yaml，期望输出由 yq 现场算，
// 保证「Go 与 bash 版行为一致」这条不变量对小配置也成立。
func TestMergeCases(t *testing.T) {
	casesDir := filepath.Join("testdata", "cases")
	entries, err := os.ReadDir(casesDir)
	if err != nil {
		t.Fatalf("读取用例目录: %v", err)
	}
	yq := yqPath(t)
	if _, err := os.Stat(yq); err != nil {
		t.Skipf("跳过：找不到 yq（%s）", yq)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(casesDir, name)
			configPath := filepath.Join(dir, "config.yaml")
			mixinPath := filepath.Join(dir, "mixin.yaml")

			cmd := exec.Command(yq, "eval-all", yqExpr, configPath, mixinPath)
			want, err := cmd.Output()
			if err != nil {
				t.Fatalf("yq 执行失败: %v", err)
			}
			got := mergeFiles(t, configPath, mixinPath)
			if string(got) != string(want) {
				t.Fatalf("与 yq 不一致\n%s\n--- want ---\n%s\n--- got ---\n%s", firstDiff(string(want), string(got)), want, got)
			}
		})
	}
}
