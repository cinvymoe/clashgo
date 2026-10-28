package kernel

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clashgo/internal/env"
)

// fakeRunner 记录被调用的 systemctl 参数，并按预设脚本回应。
type fakeRunner struct {
	calls  [][]string
	reply  func(name string, args ...string) (string, string, error)
	pidSeq []string // 依次返回的 PID，用完后固定用最后一个
	pidIdx int
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if f.reply != nil {
		out, errOut, err := f.reply(name, args...)
		return []byte(out), []byte(errOut), err
	}
	// show -p MainPID 用 pidSeq 模拟 PID 变化
	if name == "systemctl" && len(args) >= 1 && args[0] == "show" {
		pid := ""
		if len(f.pidSeq) > 0 {
			i := f.pidIdx
			if i >= len(f.pidSeq) {
				i = len(f.pidSeq) - 1
			}
			pid = f.pidSeq[i]
			f.pidIdx++
		}
		return []byte(pid + "\n"), nil, nil
	}
	return nil, nil, nil
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

// apiStub 起一个假的 mihomo 外部控制器，只为让 APIReady 能通过。
// 返回把 runtime.yaml 里 external-controller 指向它的改写函数。
func apiStub(t *testing.T) func(string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":"test"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	// 监听在 0.0.0.0 形态下，客户端会换回 127.0.0.1，正好是 httptest 的地址
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	return func(yamlText string) string {
		var out []string
		for _, line := range strings.Split(yamlText, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "external-controller:") {
				line = "external-controller: '0.0.0.0:" + port + "'"
			}
			out = append(out, line)
		}
		return strings.Join(out, "\n")
	}
}

// testEnv 在临时目录里搭一份最小可用的数据面。
// 第二个返回值是实际落盘的 runtime.yaml 文本（external-controller 已指向假 API），
// 断言内容未变化时必须拿它比对，否则会因为端口被改写而永远判定为「有变化」。
func testEnv(t *testing.T, runtimeYAML string) (*env.Env, string) {
	t.Helper()
	runtimeYAML = apiStub(t)(runtimeYAML)
	root := t.TempDir()
	for _, d := range []string{"resources/profiles", "bin"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "resources", "runtime.yaml"), []byte(runtimeYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	// Validate 会先 stat 内核二进制，测试里放个占位文件即可（实际调用由 fakeRunner 拦截）
	if err := os.WriteFile(filepath.Join(root, "bin", "mihomo"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(root, ".env")
	content := "KERNEL_NAME=mihomo\nCLASH_BASE_DIR=" + root + "\nINIT_TYPE=systemd\n"
	if err := os.WriteFile(envFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLASHGO_ENV", envFile)
	e, err := env.Load()
	if err != nil {
		t.Fatalf("加载 .env: %v", err)
	}
	return e, runtimeYAML
}

const sampleRuntime = `mixed-port: 7890
external-controller: '0.0.0.0:9090'
secret: s3cret
mode: rule
`

// TestApplySkipsWhenUnchanged 内容没变化时不应该碰内核。
func TestApplySkipsWhenUnchanged(t *testing.T) {
	e, rt := testEnv(t, sampleRuntime)
	f := &fakeRunner{pidSeq: []string{"100"}}
	m := New(e)
	m.Runner = f

	applied, err := m.Apply(context.Background(), []byte(rt))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if applied {
		t.Error("配置未变化时不应施加变更")
	}
	if f.has("systemctl", "reload") || f.has("systemctl", "restart") || f.has("systemctl", "stop") {
		t.Errorf("配置未变化时不应触发任何 systemctl 变更，实际调用: %v", f.calls)
	}
}

// TestApplyFallsBackToRestart 热重载失败时必须回退冷重启。
func TestApplyFallsBackToRestart(t *testing.T) {
	e, rt := testEnv(t, sampleRuntime)
	f := &fakeRunner{pidSeq: []string{"100", "100", "200"}}
	f.reply = func(name string, args ...string) (string, string, error) {
		if name == "systemctl" && len(args) >= 1 {
			switch args[0] {
			case "is-active":
				return "active\n", "", nil
			case "reload":
				return "", "Failed to reload", errors.New("exit 1")
			case "show":
				return "100\n", "", nil
			case "stop", "start":
				return "", "", nil
			}
		}
		if name == e.KernelPath() {
			return "test is successful", "", nil
		}
		return "", "", nil
	}
	m := New(e)
	m.Runner = f

	newCfg := strings.Replace(rt, "mode: rule", "mode: global", 1)
	applied, err := m.Apply(context.Background(), []byte(newCfg))
	if err != nil {
		t.Fatalf("Apply 应通过回退重启成功: %v", err)
	}
	if !applied {
		t.Error("配置变化后应报告已施加变更")
	}
	if !f.has("systemctl", "reload") {
		t.Error("应先尝试 systemctl reload")
	}
	if !f.has("systemctl", "stop") || !f.has("systemctl", "start") {
		t.Errorf("热重载失败后应回退 stop/start，实际调用: %v", f.calls)
	}
}

// TestApplyKeepsOldConfigWhenInvalid 校验失败时旧配置必须原样保留。
func TestApplyKeepsOldConfigWhenInvalid(t *testing.T) {
	e, rt := testEnv(t, sampleRuntime)
	f := &fakeRunner{pidSeq: []string{"100"}}
	f.reply = func(name string, args ...string) (string, string, error) {
		if name == e.KernelPath() {
			return "", "parse error", errors.New("exit 1")
		}
		if name == "systemctl" {
			return "active\n", "", nil
		}
		return "", "", nil
	}
	m := New(e)
	m.Runner = f

	_, err := m.Apply(context.Background(), []byte("mode: [broken\n"))
	if err == nil {
		t.Fatal("坏配置应返回错误")
	}
	var ve *ValidateError
	if !errors.As(err, &ve) {
		t.Fatalf("错误类型应为 ValidateError，得到 %T: %v", err, err)
	}

	got, rerr := os.ReadFile(e.ConfigRuntime())
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != rt {
		t.Errorf("旧配置必须原样保留，实际内容:\n%s", got)
	}
	if _, serr := os.Stat(e.ConfigRuntime() + ".new"); !os.IsNotExist(serr) {
		t.Error("校验失败后不应残留 .new 文件")
	}
	if f.has("systemctl", "reload") || f.has("systemctl", "restart") {
		t.Error("校验失败时不应触碰内核")
	}
}

// TestReloadDetectsPIDChange 如果 reload 实际换掉了进程，必须报告失败而不是假装成功。
func TestReloadDetectsPIDChange(t *testing.T) {
	e, _ := testEnv(t, sampleRuntime)
	f := &fakeRunner{pidSeq: []string{"100", "200"}}
	f.reply = func(name string, args ...string) (string, string, error) {
		if name == "systemctl" && len(args) >= 1 {
			switch args[0] {
			case "is-active":
				return "active\n", "", nil
			case "show":
				// 第一次 100，之后 200
				pid := "100"
				if f.pidIdx > 0 {
					pid = "200"
				}
				f.pidIdx++
				return pid + "\n", "", nil
			}
		}
		return "", "", nil
	}
	m := New(e)
	m.Runner = f

	if err := m.Reload(context.Background()); err == nil {
		t.Fatal("PID 变化时 Reload 应返回错误")
	}
}

// TestReloadNoopWhenStopped 内核没跑的时候热重载是空操作，不该算失败。
func TestReloadNoopWhenStopped(t *testing.T) {
	e, _ := testEnv(t, sampleRuntime)
	f := &fakeRunner{}
	f.reply = func(name string, args ...string) (string, string, error) {
		if name == "systemctl" && len(args) >= 1 && args[0] == "is-active" {
			return "inactive\n", "", errors.New("exit 3")
		}
		return "", "", nil
	}
	m := New(e)
	m.Runner = f

	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("内核未运行时 Reload 应为空操作，得到: %v", err)
	}
	if f.has("systemctl", "reload") {
		t.Error("内核未运行时不应调用 systemctl reload")
	}
}

// TestValidateUnsupportedProxyType 把「内核不支持该协议」从普通校验错误里区分出来。
func TestValidateUnsupportedProxyType(t *testing.T) {
	e, _ := testEnv(t, sampleRuntime)
	f := &fakeRunner{}
	f.reply = func(name string, args ...string) (string, string, error) {
		if name == e.KernelPath() {
			return "unsupport proxy type: vless", "", errors.New("exit 1")
		}
		return "", "", nil
	}
	m := New(e)
	m.Runner = f

	err := m.Validate(context.Background(), e.ConfigRuntime())
	var ue *UnsupportedProxyError
	if !errors.As(err, &ue) {
		t.Fatalf("应识别为 UnsupportedProxyError，得到 %T: %v", err, err)
	}
}

// TestProxiedPort 端口优先级：mixed-port > port > socks-port > 7890。
func TestProxiedPort(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want int
	}{
		{"mixed-port 优先", "port: 1111\nmixed-port: 2222\nsocks-port: 3333\n", 2222},
		{"退到 port", "port: 1111\nsocks-port: 3333\n", 1111},
		{"退到 socks-port", "socks-port: 3333\n", 3333},
		{"全缺省用 7890", "mode: rule\n", 7890},
		{"带引号也能解析", "mixed-port: '8080'\n", 8080},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, _ := testEnv(t, c.yaml)
			m := New(e)
			if got := m.ProxiedPort(context.Background()); got != c.want {
				t.Errorf("ProxiedPort = %d，期望 %d", got, c.want)
			}
		})
	}
}

// TestStateUnsupportedInit 非 systemd 的 init 后端直接报错退出，不尝试猜测。
func TestStateUnsupportedInit(t *testing.T) {
	e, _ := testEnv(t, sampleRuntime)
	e.InitType = "openrc"
	m := New(e)
	if _, err := m.State(context.Background()); err == nil {
		t.Fatal("非 systemd 应返回明确错误")
	}
}
