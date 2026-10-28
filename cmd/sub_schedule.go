package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"clashgo/internal/schedule"
)

// selfBinary 返回当前可执行文件的绝对路径，用于写进 systemd 单元。
// 直接读 /proc/self/exe 比 os.Executable 更稳（后者在软链调用下可能返回软链路径）。
func selfBinary() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	// 软链（clashon 之类）会解析到真实二进制，这里想要的是真实路径
	if real, err := os.Readlink("/proc/self/exe"); err == nil {
		return real, nil
	}
	return p, nil
}

func newSubScheduleCmd() *cobra.Command {
	var (
		interval string
		all      bool
		off      bool
	)
	c := &cobra.Command{
		Use:   "schedule",
		Short: "定时更新订阅（systemd --user timer）",
		Long: `定时更新订阅，用 systemd 用户实例的 timer 实现。

为什么用 systemd 而不是 crontab：
  - 有 journal 日志（journalctl --user -u clashgo-sub-update），失败看得见
  - Persistent=true，关机期间错过的触发会在开机后补跑
  - RandomizedDelaySec 打散请求，避免整点撞车

单元文件写在 ~/.config/systemd/user/clashgo-sub-update.{service,timer}，
执行 clashgo sub schedule --off 会删掉它们。`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if off {
				removed, err := schedule.Remove()
				if err != nil {
					return err
				}
				if !removed {
					gApp.Out.Dim("没有找到 clashgo 生成的定时任务，无需删除")
					return nil
				}
				gApp.Out.OK("已删除定时更新任务")
				return nil
			}

			// 不带参数：只查看状态
			if !cmd.Flags().Changed("interval") && !cmd.Flags().Changed("all") && !cmd.Flags().Changed("off") {
				st, err := schedule.Current()
				if err != nil {
					return err
				}
				printScheduleStatus(cmd, st)
				return nil
			}

			d, err := schedule.ParseInterval(interval)
			if err != nil {
				return err
			}
			bin, err := selfBinary()
			if err != nil {
				return err
			}
			spec := schedule.Spec{
				BaseDir:  gApp.Env.BaseDir,
				Binary:   bin,
				Interval: d,
				All:      all,
			}
			changed, err := schedule.Install(spec)
			if err != nil {
				return err
			}
			if changed {
				scope := "当前订阅"
				if all {
					scope = "全部订阅"
				}
				gApp.Out.OK("已写入定时任务：每 %s 更新%s", humanInterval(d), scope)
			} else {
				gApp.Out.Dim("定时任务已是最新，未修改文件")
			}
			svc, _ := schedule.ServicePath()
			tmr, _ := schedule.TimerPath()
			gApp.Out.Dim("  service: %s", svc)
			gApp.Out.Dim("  timer:   %s", tmr)
			gApp.Out.Dim("查看日志：%s", schedule.LogCommand())
			gApp.Out.Dim("立即执行一次：systemctl --user start %s", schedule.ServiceName)
			return nil
		},
	}
	c.Flags().StringVarP(&interval, "interval", "i", "6h", "更新间隔（30m / 6h / 2d）")
	c.Flags().BoolVarP(&all, "all", "a", false, "更新全部订阅（默认只更新当前订阅）")
	c.Flags().BoolVar(&off, "off", false, "停用并删除定时任务")
	return c
}

func printScheduleStatus(cmd *cobra.Command, st schedule.Status) {
	out := cmd.OutOrStdout()
	if !st.Installed {
		fmt.Fprintln(out, "⏰ 定时更新：未启用")
		fmt.Fprintln(out, "   启用：clashgo sub schedule --interval 6h")
		return
	}
	state := "已停用"
	if st.Active {
		state = "运行中"
	} else if st.Enabled {
		state = "已启用但未激活"
	}
	fmt.Fprintf(out, "⏰ 定时更新：%s\n", state)
	fmt.Fprintf(out, "   间隔：%s\n", st.Interval)
	if st.NextRun != "" {
		fmt.Fprintf(out, "   下次：%s\n", st.NextRun)
	}
	if st.LastRun != "" {
		fmt.Fprintf(out, "   上次：%s\n", st.LastRun)
	}
	fmt.Fprintf(out, "   timer：%s\n", st.Timer)
}

// humanInterval 把 6h0m0s 输出成「6 小时」。
func humanInterval(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%d 天", int(d/(24*time.Hour)))
	case d%time.Hour == 0:
		return fmt.Sprintf("%d 小时", int(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("%d 分钟", int(d/time.Minute))
	default:
		return d.String()
	}
}
