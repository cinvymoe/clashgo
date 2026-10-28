// Package app 把各个 internal 包编排成命令行为。
//
// 分层的理由：cobra 层只做参数解析与输出，所有副作用（改文件、动内核）
// 都收在这里，方便单测与复用（阶段二的 daemon 会直接调这里，而不是重复一遍）。
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"clashgo/internal/api"
	"clashgo/internal/cfg"
	"clashgo/internal/cli"
	"clashgo/internal/env"
	"clashgo/internal/install"
	"clashgo/internal/kernel"
	"clashgo/internal/livecfg"
	"clashgo/internal/mixin"
	"clashgo/internal/netutil"
	"clashgo/internal/selfupdate"
	"clashgo/internal/shellcfg"
	"clashgo/internal/sub"
	"clashgo/internal/ui"
)

// App 是所有子命令的宿主。
type App struct {
	Env    *env.Env
	Kernel *kernel.Manager
	Sub    *sub.Manager
	UI     *ui.Manager
	Out    *cli.Terminal
}

// New 构造 App。
func New(e *env.Env) *App {
	k := kernel.New(e)
	return &App{
		Env:    e,
		Kernel: k,
		Sub:    sub.New(e, k),
		UI:     ui.New(e),
		Out:    cli.NewTerminal(),
	}
}

// Live 读取当前生效的运行时配置。
func (a *App) Live() (*livecfg.Live, error) {
	return livecfg.Load(a.Env.ConfigRuntime())
}

// Mixin 读取 mixin.yaml。
func (a *App) Mixin() (*mixin.Doc, error) {
	return mixin.Load(a.Env)
}

// Client 构造 Clash REST 客户端（地址/密钥取自已生效的 runtime.yaml）。
func (a *App) Client() (*api.Client, error) {
	l, err := a.Live()
	if err != nil {
		return nil, err
	}
	return api.New(l.ExternalController, l.Secret), nil
}

// ProxyEnv 根据当前运行时配置算出要写进 proxy.env 的内容。
func (a *App) ProxyEnv() (shellcfg.Proxy, error) {
	l, err := a.Live()
	if err != nil {
		return shellcfg.Proxy{}, err
	}
	return shellcfg.Proxy{
		HTTPPort:    l.HTTPPort(),
		SocksPort:   l.SocksProxyPort(),
		MixedPort:   l.MixedPort,
		BindAddress: l.LocalBind(),
		Auth:        l.Auth(),
	}, nil
}

// SystemProxyEnabled 读 mixin 里的 _custom.system-proxy.enable。
func (a *App) SystemProxyEnabled() bool {
	d, err := a.Mixin()
	if err != nil {
		return false
	}
	return d.Bool(false, "_custom", "system-proxy", "enable")
}

// SetSystemProxyEnabled 改写 mixin 里的开关。
func (a *App) SetSystemProxyEnabled(on bool) error {
	d, err := a.Mixin()
	if err != nil {
		return err
	}
	if err := d.SetBool(on, "_custom", "system-proxy", "enable"); err != nil {
		return err
	}
	_, err = d.Save()
	return err
}

// Merge 只做合并，不落盘也不碰内核（merge --dry-run 用）。
func (a *App) Merge() ([]byte, error) {
	return sub.MergeConfig(a.Env)
}

// ApplyRuntime 合并 → 校验 → 热重载。用于 mixin/secret 改动之后。
func (a *App) ApplyRuntime(ctx context.Context) (applied bool, err error) {
	merged, err := a.Merge()
	if err != nil {
		return false, err
	}
	return a.Kernel.Apply(ctx, merged)
}

// EnsurePorts 处理端口冲突：内核没在跑、且端口被别人占着时，
// 从 mixin 里换一个随机端口并重新合并。返回是否改过配置。
func (a *App) EnsurePorts(ctx context.Context) (changed bool, err error) {
	running := a.Kernel.Running(ctx)
	d, err := a.Mixin()
	if err != nil {
		return false, err
	}
	type portKey struct{ field, yamlKey string }
	candidates := []portKey{
		{"mixed-port", "mixed-port"},
		{"port", "port"},
		{"socks-port", "socks-port"},
	}
	for _, c := range candidates {
		cur := d.Int(0, c.yamlKey)
		if cur == 0 {
			continue
		}
		if !netutil.PortInUse(cur) || running {
			continue
		}
		free, ferr := netutil.FreePort()
		if ferr != nil {
			return changed, ferr
		}
		if serr := d.SetInt(free, c.yamlKey); serr != nil {
			return changed, serr
		}
		a.Out.FailEmoji("🎯", "端口冲突：[%s] %d，已改用 %d", c.yamlKey, cur, free)
		changed = true
	}
	if !changed {
		return false, nil
	}
	if _, serr := d.Save(); serr != nil {
		return changed, serr
	}
	if _, aerr := a.ApplyRuntime(ctx); aerr != nil {
		return changed, aerr
	}
	return changed, nil
}

// On 启动内核 + 写 proxy.env + 维护 rc 托管块。
func (a *App) On(ctx context.Context) error {
	if _, err := a.EnsurePorts(ctx); err != nil {
		return err
	}
	if !a.Kernel.Running(ctx) {
		if err := a.Kernel.Start(ctx); err != nil {
			return fmt.Errorf("启动内核失败: %w", err)
		}
	}
	// 内核起来之后再读一次运行时配置，确保端口是最新的
	proxyEnv, err := a.ProxyEnv()
	if err != nil {
		return err
	}
	path, err := shellcfg.Write(proxyEnv.Render())
	if err != nil {
		return err
	}
	// 只有 mixin 里开着系统代理才写 shell 托管块，否则用户明确关掉了就别偷偷加回来
	var touched []string
	if a.SystemProxyEnabled() {
		for _, rc := range shellcfg.RcFiles() {
			changed, err := shellcfg.EnsureBlock(rc)
			if err != nil {
				return fmt.Errorf("写入 %s 失败: %w", rc, err)
			}
			if changed {
				touched = append(touched, rc)
			}
		}
		// environment.d 是「已开着的终端 / 非 shell 进程」的兼底，
		// 本机不是 systemd 时安静跳过。
		if envPath, err := shellcfg.WriteSystemdEnv(proxyEnv.Render()); err == nil && envPath != "" {
			touched = append(touched, envPath)
		}
	}
	if a.Kernel.Running(ctx) {
		a.Out.OK("已开启代理环境")
		a.Out.Dim("已写入 %s", path)
		for _, f := range touched {
			a.Out.Dim("已接管 %s", f)
		}
		a.Out.Dim("当前已打开的 shell 需执行 source <对应配置文件> 或新开终端才会生效")
	}
	return nil
}

// Off 停内核 + 删 proxy.env + 移除 rc 托管块。
func (a *App) Off(ctx context.Context) error {
	// TUN 开着时直接停内核会留下路由残留，先关掉
	if l, err := a.Live(); err == nil && l.TunEnabled() {
		if err := a.TunOff(ctx); err != nil {
			return err
		}
	}
	if a.Kernel.Running(ctx) {
		if err := a.Kernel.Stop(ctx); err != nil {
			return fmt.Errorf("停止内核失败: %w", err)
		}
	}
	if err := shellcfg.Remove(); err != nil {
		return err
	}
	for _, rc := range shellcfg.RcFiles() {
		if _, err := shellcfg.RevokeBlock(rc); err != nil {
			return fmt.Errorf("清理 %s 失败: %w", rc, err)
		}
	}
	if err := shellcfg.RemoveSystemdEnv(); err != nil {
		return err
	}
	a.Out.OK("已关闭代理环境")
	a.Out.Dim("当前已打开的 shell 里的代理变量需 source 或新开终端才会消失")
	return nil
}

// TunOn 开启 TUN 模式。
//
// 与 bash 版一致的做法：TUN 设备只能在启动时创建，所以必须先 stop、
// 改 mixin、合并、再 start。这不是热重载能覆盖的场景。
func (a *App) TunOn(ctx context.Context) error {
	if l, err := a.Live(); err == nil && l.TunEnabled() {
		a.Out.OK("Tun 状态：启用")
		return nil
	}
	d, err := a.Mixin()
	if err != nil {
		return err
	}
	if err := d.SetBool(true, "tun", "enable"); err != nil {
		return err
	}
	if _, err := d.Save(); err != nil {
		return err
	}
	merged, err := a.Merge()
	if err != nil {
		return err
	}
	if _, err := a.Kernel.WriteRuntime(ctx, merged); err != nil {
		return err
	}
	// TUN 需要重新创建设备，必须重启而不是热重载
	if err := a.Kernel.Restart(ctx); err != nil {
		return fmt.Errorf("重启内核失败: %w", err)
	}
	a.Out.OK("Tun 模式已开启")
	return nil
}

// TunOff 关闭 TUN 模式。
func (a *App) TunOff(ctx context.Context) error {
	d, err := a.Mixin()
	if err != nil {
		return err
	}
	if !d.Bool(false, "tun", "enable") {
		a.Out.Fail("Tun 状态：关闭")
		return nil
	}
	if err := d.SetBool(false, "tun", "enable"); err != nil {
		return err
	}
	if _, err := d.Save(); err != nil {
		return err
	}
	merged, err := a.Merge()
	if err != nil {
		return err
	}
	if _, err := a.Kernel.WriteRuntime(ctx, merged); err != nil {
		return err
	}
	if err := a.Kernel.Restart(ctx); err != nil {
		return fmt.Errorf("重启内核失败: %w", err)
	}
	a.Out.OK("Tun 模式已关闭")
	return nil
}

// Secret 查看或设置 Web 密钥。值为空时只查看。
func (a *App) Secret(ctx context.Context, value string) error {
	if value == "" {
		l, err := a.Live()
		if err != nil {
			return err
		}
		a.Out.OKEmoji("🔑", "当前密钥：%s", l.Secret)
		return nil
	}
	if strings.ContainsAny(value, " \t\n\"'") {
		return errors.New("密钥不要包含空格或使用引号包围")
	}
	d, err := a.Mixin()
	if err != nil {
		return err
	}
	if err := d.SetString(value, "secret"); err != nil {
		return err
	}
	if _, err := d.Save(); err != nil {
		return err
	}
	if _, err := a.ApplyRuntime(ctx); err != nil {
		return err
	}
	a.Out.OK("密钥更新成功，已热重载生效")
	return nil
}

// EditMixin 用 $EDITOR 打开 mixin，保存后合并并热重载。
func (a *App) EditMixin(ctx context.Context) error {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		editor = "vi"
	}
	cmd := exec.Command(editor, a.Env.ConfigMixin())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("编辑器退出异常: %w", err)
	}
	// 编辑器可能写坏了 YAML，这里先结构校验，避免把垃圾送进内核
	if _, err := mixin.Load(a.Env); err != nil {
		return err
	}
	changed, err := a.ApplyRuntime(ctx)
	if err != nil {
		return err
	}
	if changed {
		a.Out.OK("配置更新成功，已热重载生效")
	} else {
		a.Out.OK("配置未发生变化")
	}
	return nil
}

// Upgrade 触发内核自升级。
func (a *App) Upgrade(ctx context.Context, channel string, verbose bool) error {
	if !a.Kernel.Running(ctx) {
		if err := a.On(ctx); err != nil {
			return err
		}
	}
	c, err := a.Client()
	if err != nil {
		return err
	}
	before, _ := a.KernelVersion(ctx)
	a.Out.OKEmoji("⏳", "请求内核升级...")

	// --verbose 时边升级边把内核日志推到 stderr。
	// 升级走的是内核自己的 /upgrade，进度只在内核日志里，所以必须挂 journalctl。
	stopLogs := func() {}
	if verbose {
		stopLogs = a.followKernelLog(ctx)
	}
	upErr := c.Upgrade(ctx, channel)
	stopLogs()

	if upErr != nil {
		// 内核在「已是最新」时返回非 2xx，这里把这种情况翻译成人话
		if strings.Contains(upErr.Error(), "already using latest version") {
			a.Out.OK("已是最新版本")
			return nil
		}
		return fmt.Errorf("内核升级失败: %w", upErr)
	}
	a.Out.OK("内核升级成功")

	// 升级后内核会自己重启，等 API 回来再读版本号
	if !a.Kernel.APIReady(ctx) {
		_ = a.Kernel.Restart(ctx)
	}
	if after, err := a.KernelVersion(ctx); err == nil && after != "" {
		if before != "" && before != after {
			a.Out.Dim("版本：%s → %s", before, after)
		} else {
			a.Out.Dim("版本：%s", after)
		}
	}
	return nil
}

// Doctor 自检：把「能不能正常干活」的检查点全部过一遍。
//
// 返回的问题列表让上层决定退出码；这里不直接抛错，因为 doctor 的价值
// 在于一次列出全部问题，而不是遇到第一个就中止。
func (a *App) Doctor(ctx context.Context) []string {
	var problems []string

	// 安装目录
	if err := a.Env.CheckLayout(); err != nil {
		problems = append(problems, err.Error())
	}
	// 内核文件可执行
	if st, err := os.Stat(a.Env.KernelPath()); err != nil {
		problems = append(problems, fmt.Sprintf("内核缺失：%s", a.Env.KernelPath()))
	} else if st.Mode()&0o111 == 0 {
		problems = append(problems, fmt.Sprintf("内核没有执行权限：%s", a.Env.KernelPath()))
	}
	// 运行时配置存在且可解析
	l, err := a.Live()
	if err != nil {
		problems = append(problems, fmt.Sprintf("运行时配置不可用：%v", err))
	} else {
		// 内核 API 是否可达
		if a.Kernel.Running(ctx) {
			if !a.Kernel.APIReady(ctx) {
				problems = append(problems, "内核在运行但 API 不可达，检查 external-controller 与 secret")
			}
		} else {
			problems = append(problems, "内核未运行（clashgo on 可启动）")
		}
		// 端口冲突
		if netutil.PortInUse(l.HTTPPort()) && !a.Kernel.Running(ctx) {
			problems = append(problems, fmt.Sprintf("端口 %d 被其它进程占用，clashgo on 会自动换端口", l.HTTPPort()))
		}
		// 代理端口是否与监听一致
		if l.MixedPort == 0 && l.Port == 0 {
			problems = append(problems, "mixin 里没有配置 mixed-port / port，将回退到 7890")
		}
	}
	// 面板
	if !a.UI.Installed() {
		problems = append(problems, "zashboard 未安装（clashgo ui update 可安装）")
	}
	// 订阅
	store, _, err := a.Sub.Store()
	if err != nil {
		problems = append(problems, fmt.Sprintf("订阅清单不可读：%v", err))
	} else {
		if len(store.Profiles) == 0 {
			problems = append(problems, "还没有任何订阅（clashgo sub add <url>）")
		} else if store.Use == 0 {
			problems = append(problems, "订阅清单里没有指定当前订阅（clashgo sub use <id>）")
		} else if store.Find(store.Use) == nil {
			problems = append(problems, fmt.Sprintf("当前订阅 id=%d 在清单里不存在", store.Use))
		} else if _, err := os.Stat(a.Env.ProfilePath(store.Use)); err != nil {
			problems = append(problems, fmt.Sprintf("当前订阅文件缺失：%s", a.Env.ProfilePath(store.Use)))
		}
	}
	// 配置与订阅一致性
	if base, err := os.ReadFile(a.Env.ConfigBase()); err == nil {
		store, _, _ := a.Sub.Store()
		if store != nil && store.Use != 0 {
			if prof, err := os.ReadFile(a.Env.ProfilePath(store.Use)); err == nil {
				if string(base) != string(prof) {
					problems = append(problems, "config.yaml 与当前订阅文件内容不一致（clashgo sub use <id> 可修复）")
				}
			}
		}
	} else {
		problems = append(problems, fmt.Sprintf("config.yaml 缺失：%s", a.Env.ConfigBase()))
	}
	// 合并是否可用
	if _, err := a.Merge(); err != nil {
		problems = append(problems, fmt.Sprintf("配置合并失败：%v", err))
	}
	// shell 托管块（rc + environment.d）
	if a.SystemProxyEnabled() {
		for _, rc := range shellcfg.RcFiles() {
			data, err := os.ReadFile(rc)
			if err != nil {
				continue
			}
			if !strings.Contains(string(data), shellcfg.BlockStart) {
				problems = append(problems, fmt.Sprintf("mixin 里系统代理是开启的，但 %s 没有 clashgo 托管块", filepath.Base(rc)))
			}
		}
		if shellcfg.SystemdAvailable() {
			if p, err := shellcfg.SystemdEnvPath(); err == nil {
				if _, err := os.Stat(p); err != nil {
					problems = append(problems, "mixin 里系统代理是开启的，但没有写入 environment.d（非 shell 进程拿不到代理变量）")
				}
			}
		}
	}
	return problems
}

// Status 汇总内核状态、API 与当前订阅。
type Status struct {
	Running     bool
	State       kernel.State
	Version     string
	Mode        string
	HTTPPort    int
	SocksPort   int
	APIAddr     string
	TunEnabled  bool
	UseID       int
	UseURL      string
	SubCount    int
	SystemProxy bool
	UIInstalled bool
}

// StatusOf 收集状态信息。
// InstallReporter 把 install 包的汇报接到 CLI 输出。
type InstallReporter struct{ Out *cli.Terminal }

func (r InstallReporter) Step(name string) {
	r.Out.OKEmoji("✅", "%s", name)
}

func (r InstallReporter) Skip(name, reason string) {
	r.Out.Dim("⏭️  %s：%s", name, reason)
}

func (r InstallReporter) Detail(format string, args ...any) {
	r.Out.Dim(format, args...)
}

// Progress 实现 install.Progresser：同一行覆盖刷新。
func (r InstallReporter) Progress(label string, done, total int64) {
	const mb = 1 << 20
	if total > 0 {
		fmt.Fprintf(r.Out.Err, "\r  %s  %.1f / %.1f MB（%3d%%）",
			label, float64(done)/mb, float64(total)/mb, done*100/total)
		if done >= total {
			fmt.Fprintln(r.Out.Err)
		}
		return
	}
	fmt.Fprintf(r.Out.Err, "\r  %s  %.1f MB", label, float64(done)/mb)
}

// Install 全新安装：内核 + 面板 + 配置 + systemd unit。
func (a *App) Install(ctx context.Context, o install.Options) error {
	return install.Run(ctx, o, InstallReporter{Out: a.Out})
}

// UpgradeSelf 升级 clashgo 自身。
func (a *App) UpgradeSelf(ctx context.Context, tag string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("无法定位自身可执行文件: %w", err)
	}
	if real, rerr := os.Readlink("/proc/self/exe"); rerr == nil {
		self = real
	}
	a.Out.OKEmoji("⏳", "检查 clashgo 更新...")
	res, err := selfupdate.Update(ctx, self, selfupdate.Options{Tag: tag})
	if err != nil {
		return err
	}
	if res.UpToDate {
		a.Out.OK("已是最新版本（%s）", res.To)
		return nil
	}
	a.Out.OK("已升级：%s → %s", orDev(res.From), res.To)
	a.Out.Dim("产物：%s（%s，sha256 %s）", res.Asset, humanBytes(res.Downloaded), res.SHA256[:12])
	a.Out.Dim("旧版本备份在 %s.bak", self)
	return nil
}

// orDev 把空版本号显示成 dev。
func orDev(v string) string {
	if v == "" {
		return "dev"
	}
	return v
}

// humanBytes 输出「10.1 MB」。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// KernelVersion 读取内核版本号，读不到时返回空字符串。
func (a *App) KernelVersion(ctx context.Context) (string, error) {
	c, err := a.Client()
	if err != nil {
		return "", err
	}
	v, err := c.Version(ctx)
	if err != nil {
		return "", err
	}
	if s, ok := v["version"].(string); ok {
		return s, nil
	}
	return "", nil
}

// followKernelLog 起一个 goroutine 把 journalctl 的输出写到 stderr，
// 返回的函数用于停止它。`clashgo upgrade -v` 靠这个看到内核自己的下载进度。
func (a *App) followKernelLog(ctx context.Context) func() {
	logCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(logCtx, "journalctl", "-u", a.Env.KernelName, "-q", "-f", "-n", "0")
	cmd.Stdout = a.Out.Err
	cmd.Stderr = a.Out.Err
	if err := cmd.Start(); err != nil {
		cancel()
		a.Out.Dim("（无法跟踪内核日志：%v）", err)
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			_ = cmd.Process.Kill()
		}
	}
}

func (a *App) StatusOf(ctx context.Context) (Status, error) {
	st := Status{SystemProxy: a.SystemProxyEnabled(), UIInstalled: a.UI.Installed()}
	if state, err := a.Kernel.State(ctx); err == nil {
		st.State = state
		st.Running = state == kernel.StateRunning
	}
	if l, err := a.Live(); err == nil {
		st.Mode = l.Mode
		st.HTTPPort = l.HTTPPort()
		st.SocksPort = l.SocksProxyPort()
		st.APIAddr = l.ExternalController
		st.TunEnabled = l.TunEnabled()
	}
	if st.Running {
		if c, err := a.Client(); err == nil {
			if v, err := c.Version(ctx); err == nil {
				if s, ok := v["version"].(string); ok {
					st.Version = s
				}
			}
		}
	}
	store, _, err := a.Sub.Store()
	if err == nil && store != nil {
		st.UseID = store.Use
		st.SubCount = len(store.Profiles)
		if p := store.Find(store.Use); p != nil {
			st.UseURL = p.URL
		}
	}
	return st, nil
}

// MergeTo 把合并结果写到指定文件；path 为空时写到 stdout。
func (a *App) MergeTo(path string) error {
	merged, err := a.Merge()
	if err != nil {
		return err
	}
	if path == "" {
		_, err = os.Stdout.Write(merged)
		return err
	}
	return os.WriteFile(path, merged, 0o600)
}

// ValidateRuntime 用内核校验给定的配置文件。
func (a *App) ValidateRuntime(ctx context.Context, path string) error {
	return a.Kernel.Validate(ctx, path)
}

// FormatRuntime 解析并重新格式化运行时配置（诊断用）。
func (a *App) FormatRuntime(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	n, err := cfg.Parse(data)
	if err != nil {
		return nil, err
	}
	return cfg.Render(n)
}
