package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// alias 是「老用户肌肉记忆」的兼容入口。
//
// 从 clashctl 迁到 clashgo 之后，clashon / clashoff / clashsub 这些词仍然是
// 用户手指的第一反应。把它们做成软链比让用户改习惯更友好：
//
//	clashon      -> clashgo on
//	clashoff     -> clashgo off
//	clashrestart -> clashgo restart
//	clashstatus  -> clashgo status
//	clashsub     -> clashgo sub
//	clashsecret  -> clashgo secret
//	clashmixin   -> clashgo mixin
//	clashtun     -> clashgo tun
//	clashproxy   -> clashgo proxy
//	clashui      -> clashgo ui
//	clashlog     -> clashgo log
//	clashupgrade -> clashgo upgrade
//
// 实现方式是「软链到同一个二进制 + 按 $0 的 basename 分流」，
// 比往 PATH 里塞一堆 10MB 的副本干净得多。
var aliases = map[string]string{
	"clashon":      "on",
	"clashoff":     "off",
	"clashrestart": "restart",
	"clashstatus":  "status",
	"clashsub":     "sub",
	"clashsecret":  "secret",
	"clashmixin":   "mixin",
	"clashtun":     "tun",
	"clashproxy":   "proxy",
	"clashui":      "ui",
	"clashlog":     "log",
	"clashupgrade": "upgrade",
}

// newAliasListCmd 供 `clashgo aliases` 查询当前注册了哪些别名。
func newAliasListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "aliases",
		Short: "列出 clashctl 兼容别名",
		Long:  "列出 clashgo 提供的 clashctl 风格别名（通过软链到同一个二进制实现）。",
		RunE: func(cmd *cobra.Command, args []string) error {
			names := make([]string, 0, len(aliases))
			for n := range aliases {
				names = append(names, n)
			}
			// 稳定输出顺序，方便 diff
			for i := 0; i < len(names); i++ {
				for j := i + 1; j < len(names); j++ {
					if names[j] < names[i] {
						names[i], names[j] = names[j], names[i]
					}
				}
			}
			for _, n := range names {
				fmt.Fprintf(cmd.OutOrStdout(), "%-14s -> clashgo %s\n", n, aliases[n])
			}
			return nil
		},
	}
}
