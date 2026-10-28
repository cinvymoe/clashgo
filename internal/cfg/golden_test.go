package cfg

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// yqExpr 是从 bash 版 `_merge_config()` 原样搬过来的 yq 表达式。
// 它在这里的作用是「事实来源」：Go 合并引擎的输出必须与它逐字节一致，
// 这样从 bash 迁移到 Go 时配置不会发生任何意外变化。
const yqExpr = `
select(fileIndex==0) as $config |
select(fileIndex==1) as $mixin |
$mixin |= del(._custom) |
(($config // {}) * $mixin) as $runtime |
$runtime |
.rules = (
  ($mixin.rules.prefix // []) +
  ($config.rules // []) +
  ($mixin.rules.suffix // [])
) |
.proxies = (
  ($mixin.proxies.prefix // []) +
  (
    ($config.proxies // []) as $configList |
    ($mixin.proxies.override // []) as $overrideList |
    $configList | map(
      . as $configItem |
      (
        $overrideList[] | select(.name == $configItem.name)
      ) // $configItem
    )
  ) +
  ($mixin.proxies.suffix // [])
) |
.proxy-groups = (
  ($mixin.proxy-groups.prefix // []) +
  (
    ($config.proxy-groups // []) as $configList |
    ($mixin.proxy-groups.override // []) as $overrideList |
    $configList | map(
      . as $configItem |
      (
        $overrideList[] | select(.name == $configItem.name)
      ) // $configItem
    )
  ) +
  ($mixin.proxy-groups.suffix // [])
) |
($mixin.proxy-groups.inject // {}) as $inj |
.proxy-groups[] |= (
  . as $g |
  ($inj | .[$g.name] // []) as $extra |
  .proxies = (.proxies + $extra | unique)
)
`

// yqPath 定位 bash 版仍在用的 yq 二进制，作为黄金参考实现。
func yqPath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("BIN_YQ"); p != "" {
		return p
	}
	return filepath.Join(repoRoot(t), "bin", "yq")
}

// TestMergeGolden 把 Go 合并结果和 yq 的输出逐字节对比。
// 这是整个迁移最关键的验收点：只要它通过，从 bash 切到 Go 时 runtime.yaml 不会变。
func TestMergeGolden(t *testing.T) {
	configPath, mixinPath := loadPair(t)
	yq := yqPath(t)
	if _, err := os.Stat(yq); err != nil {
		t.Skipf("跳过：找不到 yq（%s）", yq)
	}

	cmd := exec.Command(yq, "eval-all", yqExpr, configPath, mixinPath)
	want, err := cmd.Output()
	if err != nil {
		t.Fatalf("yq 执行失败: %v", err)
	}

	got := mergeFiles(t, configPath, mixinPath)

	if string(got) != string(want) {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "want.yaml"), want, 0o644)
		_ = os.WriteFile(filepath.Join(dir, "got.yaml"), got, 0o644)
		t.Fatalf("合并结果与 yq 不一致，差异已写入 %s\n%s", dir, firstDiff(string(want), string(got)))
	}
}

// mergeFiles 读两个文件、合并、渲染，返回最终字节。
func mergeFiles(t *testing.T, configPath, mixinPath string) []byte {
	t.Helper()
	cfgNode, err := Parse(readFile(t, configPath))
	if err != nil {
		t.Fatalf("解析 %s: %v", configPath, err)
	}
	mixNode, err := Parse(readFile(t, mixinPath))
	if err != nil {
		t.Fatalf("解析 %s: %v", mixinPath, err)
	}
	merged, err := Merge(cfgNode, mixNode)
	if err != nil {
		t.Fatalf("合并: %v", err)
	}
	out, err := Render(merged)
	if err != nil {
		t.Fatalf("渲染: %v", err)
	}
	return out
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// firstDiff 返回第一处不一致的行号与上下文，方便定位。
func firstDiff(want, got string) string {
	wl := splitLines(want)
	gl := splitLines(got)
	for i := 0; i < len(wl) && i < len(gl); i++ {
		if wl[i] != gl[i] {
			return "首个差异在第 " + itoa(i+1) + " 行\n want: " + wl[i] + "\n  got: " + gl[i]
		}
	}
	if len(wl) != len(gl) {
		return "行数不同: want=" + itoa(len(wl)) + " got=" + itoa(len(gl))
	}
	return "字节不同但逐行内容相同（可能是行尾差异）"
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	out = append(out, cur)
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
