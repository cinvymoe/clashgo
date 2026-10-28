package cmd

import (
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

// newLogCmd 直接透传 journalctl，保留 -f / -n / --since 等原生参数。
func newLogCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "log [journalctl 参数...]",
		Short:              "查看内核日志（等价于 journalctl -u mihomo）",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			argv := append([]string{"-u", gApp.Env.KernelName}, args...)
			c := exec.Command("journalctl", argv...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := c.Run(); err != nil {
				// journalctl 在无日志时返回 1，这里不透传成错误
				return nil
			}
			return nil
		},
	}
}
