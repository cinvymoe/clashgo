// Package kernel 管理 mihomo 内核进程的生命周期：校验、热重载、重启、启停，以及 TUN 开关。
//
// 设计要点（详见 docs/design.md 4.4 / 4.5）：
//   - 配置永远先写临时文件、校验通过后再原子替换，内核不会看到半截配置
//   - 配置生效优先走 SIGHUP 热重载（进程不退出、不断在途连接），失败才回退重启
//   - 是否成功不靠日志文本判断，而是「PID 未变 + API 可达」双重条件
package kernel

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"clashgo/internal/env"
)

// State 是内核进程的粗粒度状态。
type State string

const (
	StateRunning     State = "running"
	StateStopped     State = "stopped"
	StateFailed      State = "failed"
	StateUnsupported State = "unsupported"
)

// Manager 是对内核的全部操作入口。所有方法都通过注入的 systemctl 路径工作，
// 方便在测试里替换成假的实现。
type Manager struct {
	Env    *env.Env
	Runner Runner // 执行 systemctl（可注入）
	Client *http.Client
}

// Runner 抽象出外部命令调用，生产环境是 exec.CommandContext。
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)
}

// execRunner 是 Runner 的默认实现。
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	var out, errBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return out.Bytes(), errBuf.Bytes(), err
}

// New 构造一个连接到默认数据面的 Manager。
func New(e *env.Env) *Manager {
	return &Manager{
		Env:    e,
		Runner: execRunner{},
		Client: &http.Client{
			Timeout: 3 * time.Second,
			Transport: &http.Transport{
				Proxy:           nil, // 内核 API 必须直连，否则启用系统代理后会绕圈
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}
}

// unit 返回 systemd 单元名。bash 版写死 mihomo，这里跟随 KERNEL_NAME。
func (m *Manager) unit() string { return m.Env.KernelName }

// ---- 状态与启停 ----

// State 查询内核是否在运行。
func (m *Manager) State(ctx context.Context) (State, error) {
	if m.Env.InitType != "systemd" {
		return StateUnsupported, fmt.Errorf("暂只支持 systemd，当前 INIT_TYPE=%q", m.Env.InitType)
	}
	out, _, err := m.Runner.Run(ctx, "systemctl", "is-active", m.unit())
	status := strings.TrimSpace(string(out))
	if err != nil && status == "" {
		return StateFailed, fmt.Errorf("查询 systemctl 状态失败: %w", err)
	}
	switch status {
	case "active", "activating", "reloading":
		return StateRunning, nil
	case "inactive", "deactivating", "failed":
		return StateStopped, nil
	default:
		return StateFailed, fmt.Errorf("未知的 systemd 状态: %q", status)
	}
}

// Running 是 State 的布尔便捷封装。
func (m *Manager) Running(ctx context.Context) bool {
	st, err := m.State(ctx)
	return err == nil && st == StateRunning
}

// Start 启动内核并等待 API 就绪。
func (m *Manager) Start(ctx context.Context) error {
	if _, _, err := m.Runner.Run(ctx, "systemctl", "start", m.unit()); err != nil {
		return fmt.Errorf("启动 %s 失败: %w", m.unit(), err)
	}
	if !m.waitReady(ctx, 5*time.Second) {
		return fmt.Errorf("%s 已启动但 API 未就绪", m.unit())
	}
	return nil
}

// Stop 停止内核。
func (m *Manager) Stop(ctx context.Context) error {
	if _, _, err := m.Runner.Run(ctx, "systemctl", "stop", m.unit()); err != nil {
		return fmt.Errorf("停止 %s 失败: %w", m.unit(), err)
	}
	return nil
}

// pid 取当前主进程 PID，取不到返回空串。
func (m *Manager) pid(ctx context.Context) string {
	out, _, err := m.Runner.Run(ctx, "systemctl", "show", "-p", "MainPID", "--value", m.unit())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// waitReady 轮询 API 直到可达或超时。
func (m *Manager) waitReady(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if m.APIReady(ctx) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
	return false
}

// ---- 配置校验 ----

// Validate 用 `mihomo -t` 校验一个配置文件。返回内核的原始输出便于定位问题。
// Validate 用内核自检给定配置。
//
// 注意 -d 必须指向 resources 目录：mihomo 找不到 Country.mmdb / geosite.dat
// 时会去外网下载，把文件放到 -d 指定的目录里。曾经用临时目录导致
// /tmp 里凭空多出 GeoSite.dat，并且测试会卡住几十秒。
func (m *Manager) Validate(ctx context.Context, path string) error {
	bin := m.Env.KernelPath()
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("找不到内核可执行文件 %s: %w", bin, err)
	}
	// 配置可能不在 resources 下（比如临时文件），但 geo 数据只在 resources 里，
	// 所以 -d 固定用 resources 目录，-f 用实际配置文件。
	dir := m.Env.ResourcesDir()
	if filepath.Dir(path) == dir {
		dir = filepath.Dir(path)
	}
	args := []string{"-d", dir, "-f", path, "-t"}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stdout, stderr, err := m.Runner.Run(ctx, bin, args...)
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(string(stdout) + "\n" + string(stderr))
	if strings.Contains(detail, "unsupport proxy type") {
		return &UnsupportedProxyError{Detail: detail}
	}
	return &ValidateError{Detail: detail, Err: err}
}

// ValidateError 表示内核拒绝了这个配置。
type ValidateError struct {
	Detail string
	Err    error
}

func (e *ValidateError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("配置校验失败: %v", e.Err)
	}
	return fmt.Sprintf("配置校验失败: %s", e.Detail)
}

func (e *ValidateError) Unwrap() error { return e.Err }

// UnsupportedProxyError 是 ValidateError 的细分：订阅里含当前内核不支持的协议。
type UnsupportedProxyError struct {
	Detail string
}

func (e *UnsupportedProxyError) Error() string {
	return "订阅中包含不受支持的代理协议，请检查并升级内核版本"
}

// ---- 写盘与生效 ----

// WriteRuntime 把新配置原子写入 runtime.yaml：
// 先写 runtime.yaml.new，校验通过后 mv 覆盖，失败则删掉临时文件、旧配置保持不变。
//
// 返回是否真的发生了变化（内容相同则返回 false，调用方据此跳过重载）。
func (m *Manager) WriteRuntime(ctx context.Context, data []byte) (changed bool, err error) {
	target := m.Env.ConfigRuntime()
	staged := target + ".new"
	if err := os.WriteFile(staged, data, 0o600); err != nil {
		return false, fmt.Errorf("写入 %s 失败: %w", staged, err)
	}
	if err := m.Validate(ctx, staged); err != nil {
		_ = os.Remove(staged)
		return false, err
	}
	old, oldErr := os.ReadFile(target)
	if oldErr == nil && md5.Sum(old) == md5.Sum(data) {
		_ = os.Remove(staged)
		return false, nil
	}
	if err := os.Rename(staged, target); err != nil {
		_ = os.Remove(staged)
		return false, fmt.Errorf("替换 %s 失败: %w", target, err)
	}
	return true, nil
}

// Reload 触发 SIGHUP 热重载，并验证「进程没换 + API 回来了」。
// 内核未运行时直接视为成功（没什么需要重载的）。
func (m *Manager) Reload(ctx context.Context) error {
	if !m.Running(ctx) {
		return nil
	}
	before := m.pid(ctx)
	if _, stderr, err := m.Runner.Run(ctx, "systemctl", "reload", m.unit()); err != nil {
		return fmt.Errorf("systemctl reload 失败: %s", strings.TrimSpace(string(stderr)))
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		// PID 变了说明这次根本不是热重载
		if after := m.pid(ctx); after != before {
			return fmt.Errorf("进程被替换（%s → %s），这不是热重载", before, after)
		}
		if m.APIReady(ctx) {
			return nil
		}
	}
	return fmt.Errorf("热重载后 API 未在 3s 内恢复")
}

// Restart 冷重启内核（会断开在途连接），用于热重载不可用时兜底。
func (m *Manager) Restart(ctx context.Context) error {
	if err := m.Stop(ctx); err != nil {
		return err
	}
	time.Sleep(100 * time.Millisecond)
	return m.Start(ctx)
}

// Apply 是「写配置 → 生效」的完整流程，对应 bash 版 `_merge_config_reload`：
//  1. 原子写入 runtime.yaml 并校验（校验失败时旧配置不动，直接返回错误）
//  2. 内容没变化则不打扰内核
//  3. 优先热重载，失败回退冷重启
//
// 返回值表示本次是否真的向内核施加了变更。
func (m *Manager) Apply(ctx context.Context, data []byte) (applied bool, err error) {
	changed, err := m.WriteRuntime(ctx, data)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	if err := m.Reload(ctx); err != nil {
		if rerr := m.Restart(ctx); rerr != nil {
			return false, fmt.Errorf("热重载失败（%v），回退重启也失败: %w", err, rerr)
		}
		return true, nil
	}
	return true, nil
}

// ---- 内核 API ----

// APIAddr 直接读 runtime.yaml 拿外部控制器地址（不做完整合并，够用且快）。
func (m *Manager) APIAddr(ctx context.Context) string {
	data, err := os.ReadFile(m.Env.ConfigRuntime())
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "external-controller:"); ok {
			return strings.Trim(strings.TrimSpace(v), `'"`)
		}
	}
	return ""
}

// APIReady 探活：请求 /version，能拿到 2xx 就算就绪。
func (m *Manager) APIReady(ctx context.Context) bool {
	addr := m.APIAddr(ctx)
	if addr == "" {
		return false
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	// 0.0.0.0 / :: 这类监听地址在客户端要换成本机回环
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := fmt.Sprintf("http://%s/version", net.JoinHostPort(host, port))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	if secret := m.secret(); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := m.Client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// secret 从 runtime.yaml 里抠出 secret，供 API 探活使用。
func (m *Manager) secret() string {
	data, err := os.ReadFile(m.Env.ConfigRuntime())
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "secret:"); ok {
			return strings.Trim(strings.TrimSpace(v), `'"`)
		}
	}
	return ""
}

// ---- 端口 ----

// ProxiedPort 返回系统代理应该指向的端口：优先 mixed-port，其次 port，再其次 socks-port。
// runtime.yaml 里三者都没有时按 mihomo 默认 7890 处理（与 bash 版 _detect_proxy_port 一致）。
func (m *Manager) ProxiedPort(ctx context.Context) int {
	data, err := os.ReadFile(m.Env.ConfigRuntime())
	if err != nil {
		return 7890
	}
	values := map[string]int{}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		for _, key := range []string{"mixed-port", "port", "socks-port"} {
			if v, ok := strings.CutPrefix(trimmed, key+":"); ok {
				if n, err := strconv.Atoi(strings.Trim(strings.TrimSpace(v), `'"`)); err == nil {
					values[key] = n
				}
			}
		}
	}
	for _, key := range []string{"mixed-port", "port", "socks-port"} {
		if n, ok := values[key]; ok && n > 0 {
			return n
		}
	}
	return 7890
}
