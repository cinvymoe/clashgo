package cmd

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"clashgo/internal/state"
)

func newSubCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "sub",
		Short: "订阅管理",
		Long: `订阅清单存在 resources/profiles.yaml，订阅文件存在 resources/profiles/<id>.yaml。

切换或更新当前订阅时，只有真的改了配置才会热重载内核，
进程不退出、在途连接不断。`,
	}
	c.AddCommand(newSubLsCmd(), newSubAddCmd(), newSubUseCmd(),
		newSubUpdateCmd(), newSubDelCmd())
	return c
}

func newSubLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "列出所有订阅",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, _, err := gApp.Sub.Store()
			if err != nil {
				return err
			}
			if len(store.Profiles) == 0 {
				gApp.Out.Fail("暂无订阅，执行 clashgo sub add <url> 添加")
				return nil
			}
			for _, p := range store.Profiles {
				mark := "  "
				if p.ID == store.Use {
					mark = "👉"
				}
				gApp.Out.Line("%s [%d] %s", mark, p.ID, p.URL)
			}
			return nil
		},
	}
}

func newSubAddCmd() *cobra.Command {
	var convert bool
	c := &cobra.Command{
		Use:   "add <url>",
		Short: "添加订阅（下载 → 校验 → 落盘）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := gApp.Sub.Add(cmd.Context(), args[0], convert)
			if err != nil {
				return err
			}
			gApp.Out.OK("订阅添加成功：[%d] %s", p.ID, p.URL)
			gApp.Out.Dim("执行 clashgo sub use %d 切换过去", p.ID)
			return nil
		},
	}
	c.Flags().BoolVarP(&convert, "convert", "c", false, "强制走 subconverter 转换")
	return c
}

func newSubUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use <id>",
		Short: "切换到指定订阅（热重载生效）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			if err := gApp.Sub.Use(cmd.Context(), id); err != nil {
				return err
			}
			gApp.Out.OK("已切换到订阅 [%d]", id)
			return nil
		},
	}
}

func newSubUpdateCmd() *cobra.Command {
	var convert bool
	var all bool
	c := &cobra.Command{
		Use:   "update [id]",
		Short: "更新订阅（默认更新当前使用的那个）",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if all {
				store, _, err := gApp.Sub.Store()
				if err != nil {
					return err
				}
				if len(store.Profiles) == 0 {
					return fmt.Errorf("暂无订阅")
				}
				var failed int
				for _, p := range store.Profiles {
					switched, err := gApp.Sub.Update(ctx, p.ID, convert)
					if err != nil {
						gApp.Out.Fail("[%d] 更新失败：%v", p.ID, err)
						failed++
						continue
					}
					if switched {
						gApp.Out.OK("[%d] 已更新并热重载生效", p.ID)
					} else {
						gApp.Out.OK("[%d] 已更新", p.ID)
					}
				}
				if failed > 0 {
					return fmt.Errorf("%d 个订阅更新失败", failed)
				}
				return nil
			}
			id := 0
			if len(args) > 0 {
				var err error
				if id, err = parseID(args[0]); err != nil {
					return err
				}
			}
			switched, err := gApp.Sub.Update(ctx, id, convert)
			if err != nil {
				return err
			}
			if switched {
				gApp.Out.OK("订阅已更新，已热重载生效")
			} else {
				gApp.Out.OK("订阅已更新（非当前订阅，配置未变动）")
			}
			return nil
		},
	}
	c.Flags().BoolVarP(&convert, "convert", "c", false, "强制走 subconverter 转换")
	c.Flags().BoolVarP(&all, "all", "a", false, "更新全部订阅")
	return c
}

func newSubDelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "del <id>",
		Short: "删除订阅（正在使用的不允许删除）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			p, err := gApp.Sub.Delete(id)
			if err != nil {
				return err
			}
			gApp.Out.OK("已删除订阅：[%d] %s", p.ID, p.URL)
			return nil
		},
	}
}

// parseID 解析订阅编号，顺便把「看起来不像数字」的错误说清楚。
func parseID(s string) (int, error) {
	id, err := strconv.Atoi(s)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("订阅编号必须是正整数，收到 %q", s)
	}
	return id, nil
}

// 让编译器别抱怨 state 只用于类型断言这类场景。
var _ = state.Profile{}
