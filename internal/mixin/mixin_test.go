package mixin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clashgo/internal/env"
)

const sampleMixin = `_custom:
  system-proxy:
    enable: true
mixed-port: 7890
external-controller: '0.0.0.0:9090'
secret: gEW3eF
allow-lan: true # 若开启务必设置用户验证以防暴露公网后被滥用
#authentication:
#  - "admin:admin123"
rules:
  prefix:
    - DOMAIN,api64.ipify.org,DIRECT
  suffix: []
tun:
  enable: false
`

func testEnv(t *testing.T) *env.Env {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "resources"), 0o755); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(root, ".env")
	if err := os.WriteFile(envFile, []byte("KERNEL_NAME=mihomo\nCLASH_BASE_DIR="+root+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLASHGO_ENV", envFile)
	e, err := env.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.ConfigMixin(), []byte(sampleMixin), 0o600); err != nil {
		t.Fatal(err)
	}
	return e
}

// TestLoadAndGet 基本读取。
func TestLoadAndGet(t *testing.T) {
	e := testEnv(t)
	d, err := Load(e)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := d.String("secret"); got != "gEW3eF" {
		t.Errorf("secret = %q", got)
	}
	if got := d.Int(0, "mixed-port"); got != 7890 {
		t.Errorf("mixed-port = %d", got)
	}
	if !d.Bool(false, "_custom", "system-proxy", "enable") {
		t.Error("system-proxy.enable 应为 true")
	}
	if d.Bool(true, "tun", "enable") {
		t.Error("tun.enable 应为 false")
	}
}

// TestSavePreservesComments 编辑一个值之后，其它注释必须原样保留。
// 这是 mixin 包最重要的性质：用户手写的说明不能丢。
func TestSavePreservesComments(t *testing.T) {
	e := testEnv(t)
	d, err := Load(e)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetString("newSecret", "secret"); err != nil {
		t.Fatal(err)
	}
	changed, err := d.Save()
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !changed {
		t.Fatal("改动了内容应报告 changed")
	}

	data, err := os.ReadFile(e.ConfigMixin())
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"secret: newSecret",
		"# 若开启务必设置用户验证以防暴露公网后被滥用",
		"#authentication:",
		`#  - "admin:admin123"`,
		"DOMAIN,api64.ipify.org,DIRECT",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("保存后丢失了 %q，实际内容:\n%s", want, text)
		}
	}
}

// TestSaveNoChangeIsNoop 内容没变时不写盘。
func TestSaveNoChangeIsNoop(t *testing.T) {
	e := testEnv(t)
	d, err := Load(e)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(e.ConfigMixin())
	changed, err := d.Save()
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("未改动内容不应报告 changed")
	}
	after, _ := os.Stat(e.ConfigMixin())
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("未改动内容不应重写文件")
	}
}

// TestSetNestedCreatesPath 设置不存在的嵌套路径时自动建中间映射。
func TestSetNestedCreatesPath(t *testing.T) {
	e := testEnv(t)
	d, err := Load(e)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetBool(false, "_custom", "system-proxy", "enable"); err != nil {
		t.Fatal(err)
	}
	out, err := d.Render()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "enable: false") {
		t.Errorf("应写入 enable: false，实际:\n%s", out)
	}
}

// TestMissingFileIsEmpty 首次使用（mixin 不存在）不应报错。
func TestMissingFileIsEmpty(t *testing.T) {
	e := testEnv(t)
	if err := os.Remove(e.ConfigMixin()); err != nil {
		t.Fatal(err)
	}
	d, err := Load(e)
	if err != nil {
		t.Fatalf("mixin 不存在时 Load 不应报错: %v", err)
	}
	if got := d.String("secret"); got != "" {
		t.Errorf("空 mixin 取值应为空，得到 %q", got)
	}
	// 能写回去
	if err := d.SetString("abc", "secret"); err != nil {
		t.Fatalf("SetString: %v", err)
	}
	if _, err := d.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(e.ConfigMixin())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "secret: abc") {
		t.Errorf("应写入 secret，实际:\n%s", data)
	}
}

// TestBadYAML 坏文件要报错而不是静默成空文档。
func TestBadYAML(t *testing.T) {
	e := testEnv(t)
	if err := os.WriteFile(e.ConfigMixin(), []byte("a: [1, 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(e); err == nil {
		t.Fatal("坏 YAML 应报错")
	}
}
