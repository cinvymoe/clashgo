package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupCmdEnv 在临时目录里搭一份最小数据面，供命令级测试使用。
func setupCmdEnv(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"resources/profiles", "bin"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"resources/config.yaml":  "proxies:\n  - {name: a, type: ss, server: 1.2.3.4, port: 8388, cipher: aes-128-gcm, password: x}\nrules:\n  - MATCH,DIRECT\n",
		"resources/mixin.yaml":   "mixed-port: 7890\nsecret: s3cret\nrules:\n  prefix:\n    - DOMAIN,api64.ipify.org,DIRECT\n",
		"resources/runtime.yaml": "mixed-port: 7890\nsecret: s3cret\nmode: rule\n",
		"bin/mihomo":             "#!/bin/sh\n",
		".env":                   "KERNEL_NAME=mihomo\nCLASH_BASE_DIR=" + root + "\nINIT_TYPE=systemd\n",
	}
	for name, content := range files {
		mode := os.FileMode(0o600)
		if name == "bin/mihomo" {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLASHGO_ENV", filepath.Join(root, ".env"))
	return root
}

// run 执行一条命令并返回错误。
func run(t *testing.T, args ...string) error {
	t.Helper()
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	return root.Execute()
}

// TestMergeCommandWritesFile merge -o 应该把合并结果写到指定文件。
func TestMergeCommandWritesFile(t *testing.T) {
	setupCmdEnv(t)
	out := filepath.Join(t.TempDir(), "out.yaml")
	if err := run(t, "merge", "-o", out); err != nil {
		t.Fatalf("merge: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "mixed-port: 7890") {
		t.Errorf("缺少 mixin 的 mixed-port:\n%s", text)
	}
	if !strings.Contains(text, "api64.ipify.org") {
		t.Errorf("缺少 rules.prefix 注入的规则:\n%s", text)
	}
	// 合并结果不应含 _custom 之类元数据
	if strings.Contains(text, "_custom") {
		t.Errorf("_custom 不该出现在合并结果里:\n%s", text)
	}
}

// TestSubLsEmpty 空清单不应该报错，只提示去添加。
func TestSubLsEmpty(t *testing.T) {
	setupCmdEnv(t)
	if err := run(t, "sub", "ls"); err != nil {
		t.Fatalf("sub ls: %v", err)
	}
}

// TestSubDelBadID 非法编号应给出可读错误。
func TestSubDelBadID(t *testing.T) {
	setupCmdEnv(t)
	err := run(t, "sub", "del", "abc")
	if err == nil {
		t.Fatal("非法编号应报错")
	}
	if !strings.Contains(err.Error(), "正整数") {
		t.Errorf("错误信息不够清楚: %v", err)
	}
}

// TestMixinCommands mixin -c / -n 分别打印原始配置与合并结果。
func TestMixinCommands(t *testing.T) {
	setupCmdEnv(t)
	if err := run(t, "mixin", "-c"); err != nil {
		t.Fatalf("mixin -c: %v", err)
	}
	if err := run(t, "mixin", "-n"); err != nil {
		t.Fatalf("mixin -n: %v", err)
	}
}

// TestProxyToggle proxy --on/--off 只改 mixin 里的开关。
func TestProxyToggle(t *testing.T) {
	root := setupCmdEnv(t)
	if err := run(t, "proxy", "--off"); err != nil {
		t.Fatalf("proxy --off: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "resources", "mixin.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "enable: false") {
		t.Errorf("mixin 未写入关闭开关:\n%s", data)
	}
}

// TestUnknownSubcommand 未知子命令应该失败。
func TestUnknownSubcommand(t *testing.T) {
	setupCmdEnv(t)
	if err := run(t, "nope"); err == nil {
		t.Fatal("未知子命令应报错")
	}
}
