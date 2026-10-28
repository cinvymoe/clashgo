package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"clashgo/internal/cli"
	"clashgo/internal/kernel"
	"clashgo/internal/netutil"
	"clashgo/internal/selfupdate"
)

func newOnCmd() *cobra.Command {
	var printEnv bool
	c := &cobra.Command{
		Use:   "on",
		Short: "开启代理（启动内核 + 写入 proxy.env 与 shell 托管块）",
		Long: `启动 mihomo，并把代理环境变量写入 ~/.config/clashgo/proxy.env。

会维护以下位置的托管块（存在才写）：
  ~/.bashrc  ~/.zshrc  ~/.bash_profile  ~/.zprofile  ~/.profile
  ~/.config/fish/config.fish（fish 语法单独处理）
以及 systemd 用户环境 ~/.config/environment.d/90-clashgo.conf
（后者是「已打开的终端 / GUI 应用 / 非 shell 进程」的兼底）。

当前已打开的 shell 仍不会自动感知，可以：
  source ~/.config/clashgo/proxy.env        # 本 shell 立即生效
  eval "$(clashgo env)"                     # 同上，不用管文件路径
  clashgo on --print-env | source /dev/stdin # 一行搞定`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return gApp.On(cmd.Context())
		},
	}
	c.Flags().BoolVar(&printEnv, "print-env", false, "开启后用 stdout 输出 export 行，便于 eval \"$(clashgo on --print-env)\"")
	oldRunE := c.RunE
	c.RunE = func(cmd *cobra.Command, args []string) error {
		if err := oldRunE(cmd, args); err != nil {
			return err
		}
		if printEnv {
			pe, err := gApp.ProxyEnv()
			if err != nil {
				return err
			}
			fmt.Print(pe.Render())
		}
		return nil
	}
	return c
}

// newEnvCmd 输出可在当前 shell 里 eval 的代理变量赋值。
// 这是「不想开新终端、也不想改 shell 用法」时最直接的途径。
func newEnvCmd() *cobra.Command {
	var shell string
	c := &cobra.Command{
		Use:   "env",
		Short: "输出代理环境变量赋值语句（供 eval / source 使用）",
		Long: `把当前生效的代理配置输出成 shell 语句。

用法：
  eval "$(clashgo env)"                          # bash / zsh / sh
  eval (clashgo env --shell fish)                 # fish
  clashgo env --shell fish | source              # fish，等价

注意：clashgo 不能直接修改你当前 shell 的环境变量（子进程无法影响父进程），
所以必须由 shell 自己 eval。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			pe, err := gApp.ProxyEnv()
			if err != nil {
				return err
			}
			content := pe.Render()
			switch shell {
			case "", "sh", "bash", "zsh", "posix":
				fmt.Print(content)
			case "fish":
				fmt.Print(fishExports(content))
			case "nu", "nushell":
				fmt.Print(nuExports(content))
			default:
				return fmt.Errorf("不支持的 shell：%s（可用 bash/zsh/sh/fish/nu）", shell)
			}
			return nil
		},
	}
	c.Flags().StringVar(&shell, "shell", "", "目标 shell：bash/zsh/sh（默认）、fish、nu")
	return c
}

// fishExports 把 `export K=V` 逐行转成 `set -gx K V`。
func fishExports(content string) string {
	var b strings.Builder
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(line, "export ") {
			continue // 注释与空行直接丢弃，避免把说明文字当命令喂给 fish
		}
		kv := strings.TrimPrefix(line, "export ")
		i := strings.Index(kv, "=")
		if i <= 0 {
			continue
		}
		k, v := kv[:i], kv[i+1:]
		// 展开 `$other` 形式的上层变量引用：fish 里 $other 会被自动展开
		fmt.Fprintf(&b, "set -gx %s %s\n", k, v)
	}
	return b.String()
}

// nuExports 输出 nushell 的 $env.NAME = "..." 形式。
func nuExports(content string) string {
	var b strings.Builder
	vals := map[string]string{}
	var order []string
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(line, "export ") {
			continue
		}
		kv := strings.TrimPrefix(line, "export ")
		i := strings.Index(kv, "=")
		if i <= 0 {
			continue
		}
		vals[kv[:i]] = kv[i+1:]
		order = append(order, kv[:i])
	}
	for _, k := range order {
		v := vals[k]
		if strings.HasPrefix(v, "$") {
			if resolved, ok := vals[strings.TrimPrefix(v, "$")]; ok {
				v = resolved
			}
		}
		fmt.Fprintf(&b, "$env.%s = %q\n", k, v)
	}
	return b.String()
}

func newOffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "off",
		Short: "关闭代理（停内核 + 清理 proxy.env 与 shell 托管块）",
		RunE: func(cmd *cobra.Command, args []string) error {
			return gApp.Off(cmd.Context())
		},
	}
}

func newRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "重启内核",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := gApp.Kernel.Restart(cmd.Context()); err != nil {
				return err
			}
			gApp.Out.OK("内核已重启")
			return nil
		},
	}
}

func newStatusCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "status",
		Short: "查看内核与订阅状态",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := gApp.StatusOf(ctx)
			if err != nil {
				return err
			}
			out := gApp.Out
			if st.Running {
				out.OKEmoji("😼", "内核状态：运行中（%s）", st.State)
			} else {
				out.FailEmoji("😾", "内核状态：未运行（%s）", st.State)
			}
			if st.Version != "" {
				out.Line("  版本：%s", st.Version)
			}
			if st.Mode != "" {
				out.Line("  模式：%s", st.Mode)
			}
			out.Line("  代理端口：HTTP %d / SOCKS %d", st.HTTPPort, st.SocksPort)
			out.Line("  控制面板：%s", st.APIAddr)
			if st.TunEnabled {
				out.Line("  Tun：开启")
			} else {
				out.Line("  Tun：关闭")
			}
			if st.SystemProxy {
				out.Line("  系统代理：开启")
			} else {
				out.Line("  系统代理：关闭")
			}
			switch {
			case st.SubCount == 0:
				out.Line("  订阅：无")
			case st.UseID == 0:
				out.Line("  订阅：%d 个，未指定当前订阅", st.SubCount)
			default:
				out.Line("  订阅：%d 个，当前使用 [%d]", st.SubCount, st.UseID)
			}
			return nil
		},
	}
	return c
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "自检：一次性列出所有能发现的问题",
		RunE: func(cmd *cobra.Command, args []string) error {
			problems := gApp.Doctor(cmd.Context())
			if len(problems) == 0 {
				gApp.Out.OK("自检通过，一切正常")
				return nil
			}
			for _, p := range problems {
				gApp.Out.Fail("%s", p)
			}
			return fmt.Errorf("发现 %d 个问题", len(problems))
		},
	}
}

func newSecretCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "secret [新密钥]",
		Short: "查看或修改 Web 控制密钥（修改后热重载生效）",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v := ""
			if len(args) > 0 {
				v = args[0]
			}
			return gApp.Secret(cmd.Context(), v)
		},
	}
}

func newMixinCmd() *cobra.Command {
	var printOnly, showRaw bool
	c := &cobra.Command{
		Use:   "mixin",
		Short: "编辑 mixin.yaml（无参数时打印内容）",
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case showRaw:
				data, err := readFileOrEmpty(gApp.Env.ConfigBase())
				if err != nil {
					return err
				}
				fmt.Print(string(data))
				return nil
			case printOnly:
				return gApp.MergeTo("")
			default:
				return gApp.EditMixin(cmd.Context())
			}
		},
	}
	c.Flags().BoolVarP(&printOnly, "dry-run", "n", false, "打印合并后的运行时配置，不改文件")
	c.Flags().BoolVarP(&showRaw, "config", "c", false, "打印原始订阅配置 config.yaml")
	return c
}

func newTunCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "tun [on|off]",
		Short: "TUN 模式开关（需要重新创建设备，会重启内核）",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			// TUN 设备只能在启动时创建，所以这里必然重启内核，不是热重载
			if len(args) == 0 {
				l, err := gApp.Live()
				if err != nil {
					return err
				}
				if l.TunEnabled() {
					gApp.Out.OK("Tun 状态：启用")
				} else {
					gApp.Out.Fail("Tun 状态：关闭")
				}
				return nil
			}
			switch args[0] {
			case "on":
				return gApp.TunOn(ctx)
			case "off":
				return gApp.TunOff(ctx)
			default:
				return fmt.Errorf("未知参数 %q，只支持 on / off", args[0])
			}
		},
	}
	return c
}

func newProxyCmd() *cobra.Command {
	var on, off bool
	c := &cobra.Command{
		Use:   "proxy [on|off]",
		Short: "系统代理开关（写入 mixin 的 _custom.system-proxy.enable）",
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case on:
				return gApp.SetSystemProxyEnabled(true)
			case off:
				return gApp.SetSystemProxyEnabled(false)
			default:
				if gApp.SystemProxyEnabled() {
					gApp.Out.OK("系统代理：开启")
				} else {
					gApp.Out.Fail("系统代理：关闭")
				}
				return nil
			}
		},
	}
	c.Flags().BoolVar(&on, "on", false, "开启系统代理")
	c.Flags().BoolVar(&off, "off", false, "关闭系统代理")
	return c
}

func newUICmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ui",
		Short: "zashboard 面板：打印地址 / 更新",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			l, err := gApp.Live()
			if err != nil {
				return err
			}
			if !gApp.Kernel.Running(ctx) {
				gApp.Out.Fail("内核未运行，请先执行 clashgo on")
				return nil
			}
			port := l.APIPort()
			gApp.Out.OKEmoji("🎛️", "面板地址：%s", gApp.UI.APIURL("127.0.0.1", port))
			if ip := netutil.LocalIP(); ip != "" {
				gApp.Out.Dim("局域网：%s", gApp.UI.APIURL(ip, port))
			}
			if !gApp.UI.Installed() {
				gApp.Out.Dim("面板未安装，执行 clashgo ui update 安装")
			}
			return nil
		},
	}
	c.AddCommand(&cobra.Command{
		Use:   "update",
		Short: "下载 zashboard 最新版本并原子替换 resources/dist",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			_ = ctx
			gApp.Out.OKEmoji("⏳", "正在下载 zashboard...")
			n, err := gApp.UI.Install()
			if err != nil {
				return err
			}
			gApp.Out.OK("面板已更新（%d 个文件）", n)
			// 面板目录变了，让内核重新读一次配置
			if _, err := gApp.ApplyRuntime(ctx); err != nil {
				gApp.Out.Dim("配置未变更，跳过重载")
			}
			return nil
		},
	})
	return c
}

func newUpgradeCmd() *cobra.Command {
	var (
		channel   string
		verbose   bool
		self      bool
		releaseCh bool
		alphaCh   bool
		selfPre   bool
		selfRepo  string
		selfTag   string
		selfDry   bool
	)
	c := &cobra.Command{
		Use:   "upgrade",
		Short: "升级内核本体，或升级 clashgo 自身",
		Long: `默认升级 mihomo 内核（走内核自己的 /upgrade 接口）。

--self 升级 clashgo 自身：从 GitHub Releases 拉最新二进制，
原子替换当前可执行文件。替换前会做版本自检，坏包不会覆盖掉能用的版本。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if self {
				if selfRepo != "" {
					if err := os.Setenv(selfupdate.RepoEnv, selfRepo); err != nil {
						return err
					}
				}
				return gApp.UpgradeSelf(cmd.Context(), selfTag, selfPre, selfDry)
			}
			// -r / -a 互斥；同时给报错而不是悄悄选一个
			if releaseCh && alphaCh {
				return fmt.Errorf("-r/--release 与 -a/--alpha 不能同时使用")
			}
			if releaseCh {
				channel = "release"
			}
			if alphaCh {
				channel = "alpha"
			}
			return gApp.Upgrade(cmd.Context(), channel, verbose)
		},
	}
	c.Flags().StringVarP(&channel, "channel", "c", "", "升级通道：release / alpha，留空沿用内核当前设置")
	c.Flags().BoolVarP(&releaseCh, "release", "r", false, "升级至稳定版（等价 --channel release）")
	c.Flags().BoolVarP(&alphaCh, "alpha", "a", false, "升级至测试版（等价 --channel alpha）")
	c.Flags().BoolVarP(&verbose, "verbose", "v", false, "输出内核升级日志")
	c.Flags().BoolVar(&self, "self", false, "升级 clashgo 自身（从 GitHub Releases）")
	c.Flags().BoolVar(&selfPre, "pre", false, "允许安装预发布版本（配合 --self）")
	c.Flags().StringVar(&selfRepo, "repo", "", "发布仓库 owner/name（配合 --self，默认 cinvymoe/clashgo）")
	c.Flags().StringVar(&selfTag, "tag", "", "指定要安装的版本（配合 --self，如 v0.2.0）")
	c.Flags().BoolVar(&selfDry, "dry-run", false, "只检查是否有新版本，不替换（配合 --self）")
	return c
}

func newMergeCmd() *cobra.Command {
	var out string
	var check bool
	c := &cobra.Command{
		Use:   "merge",
		Short: "合并 config.yaml + mixin.yaml（诊断用）",
		Long: `默认把合并结果写到 stdout，不落盘、不碰内核。

配合 -o 可导出到文件，配合 --check 用内核校验结果是否合法。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if check {
				tmp, err := gApp.Merge()
				if err != nil {
					return err
				}
				path := out
				if path == "" {
					f, ferr := writeTemp(tmp)
					if ferr != nil {
						return ferr
					}
					defer removeFile(f)
					path = f
				} else if werr := writeFile(path, tmp); werr != nil {
					return werr
				}
				if err := gApp.ValidateRuntime(ctx, path); err != nil {
					return err
				}
				gApp.Out.OK("合并结果通过内核校验")
				return nil
			}
			return gApp.MergeTo(out)
		},
	}
	c.Flags().StringVarP(&out, "output", "o", "", "输出文件（默认 stdout；接收端会自动加引号）")
	c.Flags().BoolVar(&check, "check", false, "用内核校验合并结果")
	return c
}

func newVersionCmd() *cobra.Command {
	var short bool
	c := &cobra.Command{
		Use:   "version",
		Short: "打印 clashgo 与内核版本",
		RunE: func(cmd *cobra.Command, args []string) error {
			// --short 只输出一行版本号，供 selfupdate 自检和脚本消费。
			// 它不读任何配置，因此在配置损坏时也能用。
			if short {
				// 走包级输出：此时 gApp 可能因为跳过 env.Load 而是 nil
				cli.Line("%s", Version)
				return nil
			}
			ctx := cmd.Context()
			gApp.Out.Line("clashgo %s", Version)
			st, err := gApp.StatusOf(ctx)
			if err == nil {
				if st.Version != "" {
					gApp.Out.Line("mihomo %s（%s）", st.Version, st.State)
				} else {
					gApp.Out.Line("mihomo 未运行")
				}
			}
			if state, err := gApp.Kernel.State(ctx); err == nil && state == kernel.StateUnsupported {
				gApp.Out.Dim("当前 init 类型不受支持，仅支持 systemd")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&short, "short", false, "只打印版本号（供脚本与自检使用）")
	return c
}

// Version 由 -ldflags 注入。
var Version = "dev"

// 少量文件小工具，避免每个命令都写一遍。
func readFileOrEmpty(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

func writeFile(path string, data []byte) error { return os.WriteFile(path, data, 0o600) }

// writeTemp 把内容写到临时文件，返回路径（调用方负责删）。
func writeTemp(data []byte) (string, error) {
	f, err := os.CreateTemp("", "clashgo-merge-*.yaml")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return "", err
	}
	return f.Name(), nil
}

func removeFile(path string) { _ = os.Remove(path) }
