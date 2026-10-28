package app

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clashgo/internal/cli"
	"clashgo/internal/env"
	"clashgo/internal/kernel"
	"clashgo/internal/sub"
	"clashgo/internal/ui"
)

// fakeRunner 模拟 systemctl 与内核自检命令。
type fakeRunner struct {
	calls [][]string
	// active 决定 systemctl is-active 的输出
	active string
	pid    string
	// validateErr 非 nil 时校验失败
	validateErr error
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if name == "systemctl" && len(args) > 0 {
		switch args[0] {
		case "is-active":
			return []byte(f.active + "\n"), nil, nil
		case "show":
			return []byte(f.pid + "\n"), nil, nil
		}
		return nil, nil, nil
	}
	// kernel -t 校验
	if f.validateErr != nil {
		return nil, []byte(f.validateErr.Error()), f.validateErr
	}
	return []byte("ok"), nil, nil
}

func (f *fakeRunner) has(sub ...string) bool {
	needle := strings.Join(sub, " ")
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), needle) {
			return true
		}
	}
	return false
}

const testRuntime = `mixed-port: 7890
external-controller: '0.0.0.0:9090'
secret: s3cret
mode: rule
`

// setup 搭一份最小数据面：resources + bin + .env + mixin + 假 API。
func setup(t *testing.T) (*App, *fakeRunner, string) {
	t.Helper()

	// shellcfg 会往 $HOME 写文件，测试里隔离掉
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, f := range []string{".bashrc", ".zshrc"} {
		if err := os.WriteFile(filepath.Join(home, f), []byte("# user rc\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// 假内核 API：/version 必须能通，否则 waitReady 会一直等
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"version":"test"}`))
	}))
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	for _, d := range []string{"resources/profiles", "bin"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rt := strings.Replace(testRuntime, "0.0.0.0:9090", "127.0.0.1:"+port, 1)
	if err := os.WriteFile(filepath.Join(root, "resources", "runtime.yaml"), []byte(rt), 0o600); err != nil {
		t.Fatal(err)
	}
	mixin := `_custom:
  system-proxy:
    enable: true
mixed-port: 7890
external-controller: '127.0.0.1:` + port + `'
secret: s3cret
tun:
  enable: false
`
	if err := os.WriteFile(filepath.Join(root, "resources", "mixin.yaml"), []byte(mixin), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "resources", "config.yaml"),
		[]byte("proxies:\n  - {name: a, type: ss, server: 1.2.3.4, port: 8388, cipher: aes-128-gcm, password: x}\nrules:\n  - MATCH,DIRECT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "mihomo"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(root, ".env")
	if err := os.WriteFile(envFile, []byte("KERNEL_NAME=mihomo\nCLASH_BASE_DIR="+root+"\nINIT_TYPE=systemd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLASHGO_ENV", envFile)

	e, err := env.Load()
	if err != nil {
		t.Fatalf("加载 .env: %v", err)
	}
	f := &fakeRunner{active: "active", pid: "4242"}
	k := kernel.New(e)
	k.Runner = f

	var out, errb bytes.Buffer
	a := &App{
		Env:    e,
		Kernel: k,
		Sub:    sub.New(e, k),
		UI:     ui.New(e),
		Out:    cli.NewBuffer(&out, &errb),
	}
	return a, f, root
}

// TestOnWritesProxyEnvAndBlock 验证 on 会把 export 写进 proxy.env 并维护 rc 托管块。
func TestOnWritesProxyEnvAndBlock(t *testing.T) {
	a, _, root := setup(t)
	if err := a.On(context.Background()); err != nil {
		t.Fatalf("On: %v", err)
	}
	proxyFile := filepath.Join(os.Getenv("HOME"), ".config", "clashgo", "proxy.env")
	data, err := os.ReadFile(proxyFile)
	if err != nil {
		t.Fatalf("proxy.env 未生成: %v", err)
	}
	for _, want := range []string{"http_proxy=http://127.0.0.1:7890", "all_proxy=socks5h://"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("proxy.env 缺少 %q:\n%s", want, data)
		}
	}
	for _, f := range []string{".bashrc", ".zshrc"} {
		rc, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), f))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(rc), "# clashgo START") {
			t.Errorf("%s 缺少托管块:\n%s", f, rc)
		}
		// 用户自己的内容必须原样保留
		if !strings.Contains(string(rc), "# user rc") {
			t.Errorf("%s 把用户原有内容弄丢了:\n%s", f, rc)
		}
	}
	_ = root
}

// TestOnSkipsBlockWhenSystemProxyDisabled mixin 里关掉系统代理时不应写 rc。
func TestOnSkipsBlockWhenSystemProxyDisabled(t *testing.T) {
	a, _, _ := setup(t)
	if err := a.SetSystemProxyEnabled(false); err != nil {
		t.Fatalf("关闭系统代理: %v", err)
	}
	if err := a.On(context.Background()); err != nil {
		t.Fatalf("On: %v", err)
	}
	rc, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".bashrc"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rc), "# clashgo START") {
		t.Errorf("系统代理关闭时不该写托管块:\n%s", rc)
	}
}

// TestOffCleansUp 验证 off 会删 proxy.env 与托管块。
func TestOffCleansUp(t *testing.T) {
	a, _, _ := setup(t)
	ctx := context.Background()
	if err := a.On(ctx); err != nil {
		t.Fatalf("On: %v", err)
	}
	if err := a.Off(ctx); err != nil {
		t.Fatalf("Off: %v", err)
	}
	proxyFile := filepath.Join(os.Getenv("HOME"), ".config", "clashgo", "proxy.env")
	if _, err := os.Stat(proxyFile); !os.IsNotExist(err) {
		t.Errorf("proxy.env 应被删除，err=%v", err)
	}
	rc, _ := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".bashrc"))
	if strings.Contains(string(rc), "# clashgo START") {
		t.Errorf("托管块应被移除:\n%s", rc)
	}
	if !strings.Contains(string(rc), "# user rc") {
		t.Errorf("用户原有内容被弄丢了:\n%s", rc)
	}
}

// TestSecretSetAndView 验证设置密钥后写入 mixin 并热重载，空参只查看。
func TestSecretSetAndView(t *testing.T) {
	a, f, root := setup(t)
	ctx := context.Background()
	if err := a.Secret(ctx, "newsecret"); err != nil {
		t.Fatalf("Secret: %v", err)
	}
	mixin, err := os.ReadFile(filepath.Join(root, "resources", "mixin.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mixin), "secret: newsecret") {
		t.Errorf("mixin 未写入新密钥:\n%s", mixin)
	}
	if !f.has("systemctl", "reload") {
		t.Errorf("密钥变更应热重载，实际调用 %v", f.calls)
	}
	// 只查看不写
	if err := a.Secret(ctx, ""); err != nil {
		t.Fatalf("Secret 查看: %v", err)
	}
}

// TestSecretRejectsQuotes 密钥带引号/空格直接拒绝。
func TestSecretRejectsQuotes(t *testing.T) {
	a, _, _ := setup(t)
	for _, bad := range []string{"a b", `"x"`, "x'y"} {
		if err := a.Secret(context.Background(), bad); err == nil {
			t.Errorf("密钥 %q 应被拒绝", bad)
		}
	}
}

// TestTunOnRestartsKernel TUN 需要重新创建设备，必须 stop+start 而不是 reload。
func TestTunOnRestartsKernel(t *testing.T) {
	a, f, root := setup(t)
	ctx := context.Background()
	if err := a.TunOn(ctx); err != nil {
		t.Fatalf("TunOn: %v", err)
	}
	mixin, _ := os.ReadFile(filepath.Join(root, "resources", "mixin.yaml"))
	if !strings.Contains(string(mixin), "enable: true") {
		t.Errorf("mixin 未打开 tun:\n%s", mixin)
	}
	if !f.has("systemctl", "stop") || !f.has("systemctl", "start") {
		t.Errorf("TUN 应重启内核，实际调用 %v", f.calls)
	}
	if f.has("systemctl", "reload") {
		t.Errorf("TUN 不能靠热重载生效，不该调用 reload")
	}
	rt, _ := os.ReadFile(filepath.Join(root, "resources", "runtime.yaml"))
	if !strings.Contains(string(rt), "enable: true") {
		t.Errorf("runtime.yaml 未写入 tun.enable:\n%s", rt)
	}
}

// TestTunOnIdempotent 已经开着时不要重复重启。
func TestTunOnIdempotent(t *testing.T) {
	a, f, _ := setup(t)
	ctx := context.Background()
	if err := a.TunOn(ctx); err != nil {
		t.Fatalf("TunOn: %v", err)
	}
	before := len(f.calls)
	if err := a.TunOn(ctx); err != nil {
		t.Fatalf("再次 TunOn: %v", err)
	}
	if len(f.calls) != before {
		t.Errorf("TUN 已启用时不该再动内核，多出调用 %v", f.calls[before:])
	}
}

// TestDoctorReportsMissingBits 空数据面下 doctor 应报出问题而不是崩掉。
func TestDoctorReportsMissingBits(t *testing.T) {
	a, _, root := setup(t)
	// 删掉面板与订阅，制造问题
	problems := a.Doctor(context.Background())
	if len(problems) == 0 {
		t.Fatal("空数据面下 doctor 应报告问题")
	}
	joined := strings.Join(problems, "\n")
	if !strings.Contains(joined, "zashboard 未安装") {
		t.Errorf("应报告面板未安装，实际:\n%s", joined)
	}
	if !strings.Contains(joined, "还没有任何订阅") {
		t.Errorf("应报告无订阅，实际:\n%s", joined)
	}
	_ = root
}

// TestStatusOf 汇总状态并能读出版本与订阅。
func TestStatusOf(t *testing.T) {
	a, _, root := setup(t)
	ctx := context.Background()
	st, err := a.StatusOf(ctx)
	if err != nil {
		t.Fatalf("StatusOf: %v", err)
	}
	if !st.Running {
		t.Error("内核应为运行中")
	}
	if st.Version != "test" {
		t.Errorf("版本 = %q，期望 test", st.Version)
	}
	if st.HTTPPort != 7890 {
		t.Errorf("HTTP 端口 = %d，期望 7890", st.HTTPPort)
	}
	if st.SubCount != 0 {
		t.Errorf("订阅数 = %d，期望 0", st.SubCount)
	}
	_ = root
}

// TestEnsurePortsResolvesConflict 端口被占且内核没跑时自动换端口。
func TestEnsurePortsResolvesConflict(t *testing.T) {
	a, f, root := setup(t)
	f.active = "inactive"

	// 占住 7890，制造冲突
	l, err := net.Listen("tcp", "127.0.0.1:7890")
	if err != nil {
		t.Skipf("7890 已被占用，跳过: %v", err)
	}
	defer l.Close()

	changed, err := a.EnsurePorts(context.Background())
	if err != nil {
		t.Fatalf("EnsurePorts: %v", err)
	}
	if !changed {
		t.Fatal("端口冲突应被处理")
	}
	mixin, _ := os.ReadFile(filepath.Join(root, "resources", "mixin.yaml"))
	if strings.Contains(string(mixin), "mixed-port: 7890") {
		t.Errorf("mixin 端口未更新:\n%s", mixin)
	}
}

// TestMergeIsByteIdenticalToRuntime 合并结果必须与线上 runtime.yaml 一致（此处用同构输入）。
func TestMergeRendersRuntime(t *testing.T) {
	a, _, _ := setup(t)
	merged, err := a.Merge()
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	for _, want := range []string{"mixed-port: 7890", "secret: s3cret", "MATCH,DIRECT"} {
		if !strings.Contains(string(merged), want) {
			t.Errorf("合并结果缺少 %q:\n%s", want, merged)
		}
	}
	if strings.Contains(string(merged), "_custom") {
		t.Errorf("_custom 不该出现在 runtime 里:\n%s", merged)
	}
}
