package cmd

import (
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"clashgo/internal/app"
	"clashgo/internal/cli"
	"clashgo/internal/env"
)

var (
	// gApp 由 PersistentPreRunE 填充，子命令直接用。
	gApp    *app.App
	baseDir string
)

// Execute 是 main 的唯一入口，返回进程退出码。
func Execute() int {
	// 别名必须在 cobra 解析参数**之前**改写：
	// `clashsub ls` 里的 ls 是 sub 的子命令，如果等 PersistentPreRunE 才分流，
	// cobra 已经先把 ls 当成根命令的子命令去找并报 unknown command 了。
	rewriteAliasArgs()

	root := newRootCmd()
	if err := root.Execute(); err != nil {
		cli.Error("%v", err)
		return 1
	}
	return 0
}

// rewriteAliasArgs 把 `clashon` 这类别名调用改写成 `clashgo on`。
// 直接原地改 os.Args，后面所有逻辑（包括 cobra 与子命令里的 args）都无需感知别名。
func rewriteAliasArgs() {
	self := filepath.Base(os.Args[0])
	target, ok := aliases[self]
	if !ok {
		return
	}
	os.Args[0] = "clashgo"
	os.Args = append([]string{"clashgo", target}, os.Args[1:]...)
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "clashgo",
		Short: "用 Go 管理 mihomo 与 zashboard（与 clashctl 共用同一份配置）",
		Long: `clashgo 是 clashctl 的 Go 重写版。

它读的是同一份 .env、同一个 resources/、同一个 mihomo.service，
所以 clashctl 和 clashgo 可以混着用，看到的是同一份状态。`,
		// 让子命令自己决定输出，cobra 不打印 usage 和 error
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if baseDir != "" {
				if err := os.Setenv("CLASHGO_BASE_DIR", baseDir); err != nil {
					return err
				}
			}
			e, err := env.Load()
			if err != nil {
				return err
			}
			gApp = app.New(e)
			return nil
		},
	}
	root.PersistentFlags().StringVar(&baseDir, "base-dir", "",
		"安装目录（默认读 .env 里的 CLASH_BASE_DIR）")

	root.AddCommand(
		newOnCmd(),
		newOffCmd(),
		newRestartCmd(),
		newStatusCmd(),
		newDoctorCmd(),
		newSubCmd(),
		newMixinCmd(),
		newSecretCmd(),
		newTunCmd(),
		newProxyCmd(),
		newEnvCmd(),
		newUICmd(),
		newLogCmd(),
		newUpgradeCmd(),
		newMergeCmd(),
		newVersionCmd(),
		newAliasListCmd(),
	)
	return root
}
