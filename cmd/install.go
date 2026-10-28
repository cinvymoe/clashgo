package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"clashgo/internal/cli"
	"clashgo/internal/install"
)

func newInstallCmd() *cobra.Command {
	var o install.Options
	c := &cobra.Command{
		Use:   "install",
		Short: "全新安装：内核 + zashboard 面板 + systemd unit",
		Long: `在一台干净机器上从零搭起整套环境。

对应 clashctl 的 scripts/preflight.sh，但只支持 systemd（bash 版还有
SysVinit / OpenRC / runit / nohup 四个后端，共 70 行纯 shell）。

安装会做这些事：
  1. 前置检查（root、systemd、curl/tar/gzip）
  2. 建目录骨架 bin/ resources/ resources/profiles/ resources/zip/
  3. 下载 mihomo 内核并自检（按 CPU 的 AVX2/SSE4.2 自动选 v1/v2/v3 档）
  4. 下载 zashboard dist.zip 并解压到 resources/dist（失败不致命）
  5. 生成 mixin.yaml（含随机密钥）与 .env
  6. 写 /etc/systemd/system/mihomo.service 并 enable

装完之后：
  clashgo sub add <订阅链接>    # 添加订阅
  clashgo sub use 1             # 切换并热重载
  clashgo on                    # 启动内核 + 写代理环境变量`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// 注意：PersistentPreRunE 对 install 短路，所以这里 gApp 是 nil，
			// 汇报直接走包级 cli 输出。
			if o.BaseDir == "" {
				o.BaseDir = install.DefaultBaseDir
			}
			return install.Run(cmd.Context(), o, installReporter{})
		},
	}
	c.Flags().StringVar(&o.BaseDir, "dir", install.DefaultBaseDir, "安装目录")
	c.Flags().StringVar(&o.KernelURL, "kernel-url", "", "内核下载地址（留空按架构自动推断）")
	c.Flags().StringVar(&o.UIURL, "ui-url", "", "面板 dist.zip 地址（留空用官方发布）")
	c.Flags().StringVar(&o.Secret, "secret", "", "Web 控制密钥（留空随机生成）")
	c.Flags().IntVar(&o.MixedPort, "mixed-port", 7890, "HTTP/SOCKS 混合端口")
	c.Flags().IntVar(&o.APIPort, "api-port", 9090, "external-controller 端口")
	c.Flags().BoolVar(&o.Force, "force", false, "安装目录已存在时覆盖")
	c.Flags().BoolVar(&o.DryRun, "dry-run", false, "只打印计划，不落盘")
	c.Flags().StringVar(&o.GhProxy, "gh-proxy", "", "GitHub 下载加速前缀，如 https://ghproxy.com（也可用 URL_GH_PROXY 环境变量）")
	return c
}

// installReporter 把 install 包的汇报打到 stdout。
type installReporter struct{}

func (installReporter) Step(name string) {
	cli.OKEmoji("✅", "%s", name)
}

func (installReporter) Skip(name, reason string) {
	cli.Dim("⏭️  %s：%s", name, reason)
}

func (installReporter) Detail(format string, args ...any) {
	cli.Dim(format, args...)
}

// Progress 在同一行覆盖刷新，避免几百行进度把终端刷满。
func (installReporter) Progress(label string, done, total int64) {
	const mb = 1 << 20
	if total > 0 {
		fmt.Fprintf(os.Stderr, "\r\033[2K  %s  %.1f / %.1f MB（%3d%%）",
			label, float64(done)/mb, float64(total)/mb, done*100/total)
	} else {
		fmt.Fprintf(os.Stderr, "\r\033[2K  %s  %.1f MB", label, float64(done)/mb)
	}
	if done >= total && total > 0 {
		fmt.Fprint(os.Stderr, "\n")
	}
}
