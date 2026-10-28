package shellcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderProxyEnv 生成的 export 行必须与 bash 版 _set_system_proxy 的语义一致。
func TestRenderProxyEnv(t *testing.T) {
	p := Proxy{HTTPPort: 7890, SocksPort: 0, MixedPort: 7890, BindAddress: "127.0.0.1"}
	out := p.Render()
	for _, want := range []string{
		"export http_proxy=http://127.0.0.1:7890",
		"export https_proxy=http://127.0.0.1:7890",
		"export all_proxy=socks5h://127.0.0.1:7890",
		"export no_proxy=localhost,127.0.0.1,::1",
		"export HTTP_PROXY=$http_proxy",
		"export NO_PROXY=$no_proxy",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %q，实际内容:\n%s", want, out)
		}
	}
}

// TestRenderWithAuth authentication 存在时要带 user:pass@ 前缀。
func TestRenderWithAuth(t *testing.T) {
	p := Proxy{MixedPort: 7890, Auth: "admin:secret"}
	out := p.Render()
	if !strings.Contains(out, "http://admin:secret@127.0.0.1:7890") {
		t.Errorf("http 代理缺少认证信息:\n%s", out)
	}
	if !strings.Contains(out, "socks5h://admin:secret@127.0.0.1:7890") {
		t.Errorf("socks 代理缺少认证信息:\n%s", out)
	}
}

// TestRenderAllowLanBind allow-lan 场景下 bind-address 是真实局域网 IP，要原样用。
func TestRenderAllowLanBind(t *testing.T) {
	p := Proxy{MixedPort: 7890, BindAddress: "192.168.1.10"}
	if out := p.Render(); !strings.Contains(out, "http://192.168.1.10:7890") {
		t.Errorf("应使用 bind-address，实际:\n%s", out)
	}
}

// TestEnsureBlockCreates 首次写入托管块。
func TestEnsureBlockCreates(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".bashrc")
	if err := os.WriteFile(rc, []byte("# existing\nexport FOO=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wrote, err := EnsureBlock(rc)
	if err != nil {
		t.Fatalf("EnsureBlock: %v", err)
	}
	if !wrote {
		t.Error("首次调用应写入文件")
	}
	data, _ := os.ReadFile(rc)
	text := string(data)
	if !strings.HasPrefix(text, "# existing\nexport FOO=1\n") {
		t.Errorf("不应破坏原有内容，实际:\n%s", text)
	}
	if !strings.Contains(text, BlockStart) || !strings.Contains(text, BlockEnd) {
		t.Errorf("缺少托管块标记:\n%s", text)
	}
	if !strings.Contains(text, `.config/clashgo/proxy.env`) {
		t.Errorf("托管块应 source proxy.env:\n%s", text)
	}
}

// TestEnsureBlockIdempotent 重复调用不应不断追加。
func TestEnsureBlockIdempotent(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	if err := os.WriteFile(rc, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureBlock(rc); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(rc)
	wrote, err := EnsureBlock(rc)
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Error("第二次调用不应写入文件")
	}
	second, _ := os.ReadFile(rc)
	if string(first) != string(second) {
		t.Errorf("内容发生变化:\n第一次:\n%s\n第二次:\n%s", first, second)
	}
	if strings.Count(string(second), BlockStart) != 1 {
		t.Errorf("托管块应只有一个，实际:\n%s", second)
	}
}

// TestEnsureBlockReplacesOld 旧的 clashctl 块不应被误删，但 clashgo 块要能替换自己。
func TestEnsureBlockReplacesOld(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".bashrc")
	content := "# clashctl START\n. /data/project/clashctl/scripts/cmd/clashctl.sh\n# clashctl END\n" +
		"# clashgo START\n. /old/path\n# clashgo END\n"
	if err := os.WriteFile(rc, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureBlock(rc); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(rc)
	text := string(data)
	if !strings.Contains(text, "clashctl.sh") {
		t.Errorf("不应动 clashctl 的托管块:\n%s", text)
	}
	if strings.Count(text, BlockStart) != 1 {
		t.Errorf("clashgo 块应只有一份:\n%s", text)
	}
	if strings.Contains(text, "/old/path") {
		t.Errorf("旧内容应被替换:\n%s", text)
	}
}

// TestRevokeBlock 移除托管块并保持其它内容不变。
func TestRevokeBlock(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".bashrc")
	if err := os.WriteFile(rc, []byte("export FOO=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureBlock(rc); err != nil {
		t.Fatal(err)
	}
	removed, err := RevokeBlock(rc)
	if err != nil {
		t.Fatalf("RevokeBlock: %v", err)
	}
	if !removed {
		t.Error("应报告移除了块")
	}
	data, _ := os.ReadFile(rc)
	if strings.Contains(string(data), BlockStart) {
		t.Errorf("托管块应已删除:\n%s", data)
	}
	if !strings.Contains(string(data), "export FOO=1") {
		t.Errorf("原有内容应保留:\n%s", data)
	}
	// 幂等
	if removed, err := RevokeBlock(rc); err != nil || removed {
		t.Errorf("再次调用应为无操作，得到 removed=%v err=%v", removed, err)
	}
}

// TestRevokeBlockBroken 只有头没有尾的文件不应让后续内容整段丢失之外还报错。
func TestRevokeBlockBroken(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".bashrc")
	if err := os.WriteFile(rc, []byte("keep\n"+BlockStart+"\n. something\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RevokeBlock(rc); err != nil {
		t.Fatalf("头尾不匹配也应能处理: %v", err)
	}
	data, _ := os.ReadFile(rc)
	if !strings.Contains(string(data), "keep") {
		t.Errorf("块之前的内容应保留:\n%s", data)
	}
	if strings.Contains(string(data), BlockStart) {
		t.Errorf("残缺的块应被清掉:\n%s", data)
	}
}
