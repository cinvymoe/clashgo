package cfg

import (
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// mergeCase 读取一个用例目录并返回合并后的根节点。
func mergeCase(t *testing.T, name string) *yaml.Node {
	t.Helper()
	dir := filepath.Join("testdata", "cases", name)
	cfgNode, err := Parse(readFile(t, filepath.Join(dir, "config.yaml")))
	if err != nil {
		t.Fatalf("解析 config: %v", err)
	}
	mixNode, err := Parse(readFile(t, filepath.Join(dir, "mixin.yaml")))
	if err != nil {
		t.Fatalf("解析 mixin: %v", err)
	}
	merged, err := Merge(cfgNode, mixNode)
	if err != nil {
		t.Fatalf("合并: %v", err)
	}
	return merged
}

// strings 把序列节点里的标量取成字符串切片，非标量忽略。
func strings(n *yaml.Node) []string {
	var out []string
	for _, it := range seqItems(n) {
		out = append(out, scalarValue(it))
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSemantics 直接断言合并语义本身，不依赖 yq。
// 这样即使将来不再需要与 bash 版对照，这些规则依然被测试守着。
func TestSemantics(t *testing.T) {
	t.Run("深合并保留订阅侧独有字段", func(t *testing.T) {
		m := mergeCase(t, "deep_merge")
		dns := mapGet(m, "dns")
		if got := scalarValue(mapGet(dns, "listen")); got != "0.0.0.0:1053" {
			t.Errorf("mixin 应覆盖 listen，得到 %q", got)
		}
		if got := scalarValue(mapGet(dns, "enable")); got != "true" {
			t.Errorf("订阅侧独有字段 enable 应保留，得到 %q", got)
		}
		if got := strings(mapGet(dns, "nameserver")); !equalStrings(got, []string{"1.1.1.1"}) {
			t.Errorf("订阅侧独有字段 nameserver 应保留，得到 %v", got)
		}
		if got := strings(mapGet(dns, "fallback")); !equalStrings(got, []string{"8.8.8.8"}) {
			t.Errorf("mixin 新增字段 fallback 应出现，得到 %v", got)
		}
	})

	t.Run("列表顺序为 prefix+config+suffix", func(t *testing.T) {
		m := mergeCase(t, "list_order")
		if got := strings(mapGet(m, "rules")); !equalStrings(got, []string{"RULE,PRE1", "RULE,PRE2", "RULE,A", "RULE,B", "RULE,SUF"}) {
			t.Errorf("rules 顺序不对: %v", got)
		}
		if got := len(seqItems(mapGet(m, "proxies"))); got != 3 {
			t.Errorf("proxies 应有 3 项（prefix 1 + config 1 + suffix 1），得到 %d", got)
		}
		if got := len(seqItems(mapGet(m, "proxy-groups"))); got != 3 {
			t.Errorf("proxy-groups 应有 3 项（prefix 1 + config 1 + suffix 1），得到 %d", got)
		}
	})

	t.Run("override 按 name 替换且未命中保留", func(t *testing.T) {
		m := mergeCase(t, "override")
		proxies := seqItems(mapGet(m, "proxies"))
		if len(proxies) != 2 {
			t.Fatalf("proxies 应有 2 项，得到 %d", len(proxies))
		}
		if got := scalarValue(mapGet(proxies[0], "server")); got != "new" {
			t.Errorf("a 应被 override 替换，server=%q", got)
		}
		if got := scalarValue(mapGet(proxies[1], "server")); got != "keep" {
			t.Errorf("b 未命中 override，server 应保留为 keep，得到 %q", got)
		}
		groups := seqItems(mapGet(m, "proxy-groups"))
		if got := scalarValue(mapGet(groups[0], "type")); got != "fallback" {
			t.Errorf("G1 应被整体替换为 fallback，得到 %q", got)
		}
		if got := scalarValue(mapGet(groups[1], "type")); got != "select" {
			t.Errorf("G2 未命中 override，应保持 select，得到 %q", got)
		}
	})

	t.Run("inject 追加节点并保序去重", func(t *testing.T) {
		m := mergeCase(t, "inject")
		groups := seqItems(mapGet(m, "proxy-groups"))
		if len(groups) != 3 {
			t.Fatalf("proxy-groups 应有 3 项，得到 %d", len(groups))
		}
		byName := map[string]*yaml.Node{}
		for _, g := range groups {
			byName[scalarValue(mapGet(g, "name"))] = g
		}
		// A 原有 [x, y, x]（重复），注入 [z, x] → [x, y, z]
		if got := strings(mapGet(byName["A"], "proxies")); !equalStrings(got, []string{"x", "y", "z"}) {
			t.Errorf("A 的 proxies 应为 [x y z]，得到 %v", got)
		}
		// C 原有 [q]，注入 [z, w]
		if got := strings(mapGet(byName["C"], "proxies")); !equalStrings(got, []string{"q", "z", "w"}) {
			t.Errorf("C 的 proxies 应为 [q z w]，得到 %v", got)
		}
		// B 没有 proxies 字段，也会被补成空序列（yq 的副作用）
		if got := mapIndex(byName["B"], "proxies"); got < 0 {
			t.Error("B 应被补上 proxies 字段（即使为空）")
		}
	})

	t.Run("_custom 不进入 runtime", func(t *testing.T) {
		m := mergeCase(t, "custom_removed")
		if mapIndex(m, "_custom") >= 0 {
			t.Error("_custom 必须被剔除")
		}
		if got := scalarValue(mapGet(m, "mixed-port")); got != "7890" {
			t.Errorf("mixed-port 应为 7890，得到 %q", got)
		}
	})
}

// BenchmarkMerge 对比 Go 合并与 yq 子进程合并的耗时，
// 用真实订阅配置，规模与线上一致。
func BenchmarkMerge(b *testing.B) {
	t := &testing.T{}
	configPath, mixinPath := loadPair(t)
	cfgData := readFile(t, configPath)
	mixData := readFile(t, mixinPath)

	b.Run("go", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			cn, err := Parse(cfgData)
			if err != nil {
				b.Fatal(err)
			}
			mn, err := Parse(mixData)
			if err != nil {
				b.Fatal(err)
			}
			merged, err := Merge(cn, mn)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := Render(merged); err != nil {
				b.Fatal(err)
			}
		}
	})
}
