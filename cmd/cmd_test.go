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

// runCapture 执行命令并把写到 os.Stdout 的内容抓回来。
// env 命令直接 fmt.Print 到 stdout（要能被 eval 管道吃走），所以必须重定向 os.Stdout。
func runCapture(t *testing.T, args ...string) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(os.Stdout)
	root.SetErr(os.Stdout)
	err = root.Execute()
	w.Close()
	os.Stdout = old
	return <-done, err
}

// TestEnvCommandShellForms 三种 shell 语法都要能输出，且不能把注释当命令。
func TestEnvCommandShellForms(t *testing.T) {
	setupCmdEnv(t)
	cases := []struct {
		shell string
		want  string
	}{
		{"", "export http_proxy=http://127.0.0.1:7890"},
		{"bash", "export http_proxy=http://127.0.0.1:7890"},
		{"zsh", "export no_proxy=localhost,127.0.0.1,::1"},
		{"fish", "set -gx http_proxy http://127.0.0.1:7890"},
		{"nu", `$env.http_proxy = "http://127.0.0.1:7890"`},
	}
	for _, c := range cases {
		args := []string{"env"}
		if c.shell != "" {
			args = append(args, "--shell", c.shell)
		}
		out, err := runCapture(t, args...)
		if err != nil {
			t.Fatalf("shell=%q: %v", c.shell, err)
		}
		if !strings.Contains(out, c.want) {
			t.Errorf("shell=%q 输出缺少 %q:\n%s", c.shell, c.want, out)
		}
		if strings.Contains(out, "# #") {
			t.Errorf("shell=%q 输出把注释又注释了一次:\n%s", c.shell, out)
		}
	}
	if _, err := runCapture(t, "env", "--shell", "tcsh"); err == nil {
		t.Error("不支持的 shell 应报错")
	}
}
