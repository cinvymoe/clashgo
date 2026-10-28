package shellcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRcFilesCoversAllShells 常用 shell 的启动文件都应被纳入。
func TestRcFilesCoversAllShells(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	want := []string{".bashrc", ".zshrc", ".bash_profile", ".zprofile", ".profile"}
	for _, name := range want {
		if err := os.WriteFile(filepath.Join(home, name), []byte("# "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fish := filepath.Join(home, ".config", "fish")
	if err := os.MkdirAll(fish, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fish, "config.fish"), []byte("# fish\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := RcFiles()
	if len(got) != len(want)+1 {
		t.Fatalf("应返回 %d 个文件，实际 %d: %v", len(want)+1, len(got), got)
	}
	for i, name := range want {
		if filepath.Base(got[i]) != name {
			t.Errorf("第 %d 个应为 %s，实际 %s", i, name, got[i])
		}
	}
	if filepath.Base(got[len(got)-1]) != "config.fish" {
		t.Errorf("最后应为 config.fish，实际 %s", got[len(got)-1])
	}
}

// TestRcFilesSkipsMissing 不存在的启动文件不能凭空创建。
func TestRcFilesSkipsMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	if got := RcFiles(); len(got) != 0 {
		t.Fatalf("空 home 应返回空列表，实际 %v", got)
	}
}

// TestFishBlockSyntax fish 不能用 POSIX 的 export，必须是 set -gx 形态。
func TestFishBlockSyntax(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	path := filepath.Join(home, ".config", "fish", "config.fish")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# user config\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := EnsureBlock(path)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("首次写入应返回 changed=true")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	if !strings.Contains(out, BlockStart) || !strings.Contains(out, BlockEnd) {
		t.Fatalf("缺少托管块标记:\n%s", out)
	}
	if strings.Contains(out, "\nexport ") || strings.HasPrefix(out, "export ") {
		t.Errorf("fish 配置里不应出现 export 语句:\n%s", out)
	}
	if !strings.Contains(out, "set -gx") || !strings.Contains(out, "string split") {
		t.Errorf("fish 块应使用 set -gx 逐行转换:\n%s", out)
	}
	if !strings.Contains(out, "# user config") {
		t.Errorf("原有内容被破坏:\n%s", out)
	}
}

// TestFishIdempotent 重复写入不改变文件。
func TestFishIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	path := filepath.Join(home, ".config", "fish", "config.fish")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureBlock(path); err != nil {
		t.Fatal(err)
	}
	changed, err := EnsureBlock(path)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("第二次写入应返回 changed=false")
	}
}

// TestRenderSystemdEnv environment.d 只接受 KEY=VALUE，且必须展开 $var 引用。
func TestRenderSystemdEnv(t *testing.T) {
	proxyContent := Proxy{HTTPPort: 7890, MixedPort: 7890, BindAddress: "127.0.0.1"}.Render()
	out := renderSystemdEnv(proxyContent)
	if !strings.Contains(out, systemdEnvMarker) {
		t.Error("缺少生成标记，off 时无法安全识别")
	}
	if strings.Contains(out, "export ") {
		t.Errorf("environment.d 不接受 export:\n%s", out)
	}
	for _, want := range []string{
		"http_proxy=http://127.0.0.1:7890",
		"HTTP_PROXY=http://127.0.0.1:7890", // $http_proxy 必须被展开
		"all_proxy=socks5h://127.0.0.1:7890",
		"ALL_PROXY=socks5h://127.0.0.1:7890",
		"no_proxy=localhost,127.0.0.1,::1",
		"NO_PROXY=localhost,127.0.0.1,::1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %q，实际:\n%s", want, out)
		}
	}
	if strings.Contains(out, "$http_proxy") {
		t.Errorf("不应残留未展开的变量引用:\n%s", out)
	}
}

// TestWriteAndRemoveSystemdEnv 写入 → 幂等 → 删除的完整链路。
func TestWriteAndRemoveSystemdEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	path, err := SystemdEnvPath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, filepath.Join("environment.d", systemdEnvFile)) {
		t.Fatalf("路径不对: %s", path)
	}

	content := Proxy{HTTPPort: 7890, MixedPort: 7890}.Render()
	written, err := WriteSystemdEnv(content)
	if err != nil {
		t.Fatal(err)
	}
	if !SystemdAvailable() {
		if written != "" {
			t.Fatalf("非 systemd 环境应返回空路径，实际 %q", written)
		}
		return
	}
	if written != path {
		t.Fatalf("返回路径不符: %s vs %s", written, path)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 幂等：内容相同不重写
	if _, err := WriteSystemdEnv(content); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Error("内容相同的重复写入不应改变文件")
	}

	if err := RemoveSystemdEnv(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("off 后 environment.d 文件应被删除")
	}
	// 幂等
	if err := RemoveSystemdEnv(); err != nil {
		t.Fatalf("重复删除应幂等: %v", err)
	}
}

// TestRemoveSystemdEnvKeepsForeignFile 不是我们写的文件不能碰。
func TestRemoveSystemdEnvKeepsForeignFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	path, err := SystemdEnvPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	mine := "http_proxy=http://127.0.0.1:1080\n"
	if err := os.WriteFile(path, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSystemdEnv(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("用户手写的文件被误删: %v", err)
	}
	if string(data) != mine {
		t.Error("用户手写的文件被改动")
	}
}
