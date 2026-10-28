// Package cli 是命令行输出的统一入口。
//
// 对应 bash 版的 _okcat / _failcat / _error_quit：统一 emoji 前缀与
// 24bit 前景色，成功走 stdout、失败走 stderr，便于脚本区分。
package cli

import (
	"fmt"
	"io"
	"os"
)

// 与 bash 版一致的配色
const (
	colorOK   = "\033[38;2;200;214;229m" // #c8d6e5
	colorFail = "\033[38;2;253;121;168m" // #fd79a8
	colorErr  = "\033[38;2;249;47;96m"   // #f92f60
	colorDim  = "\033[38;2;128;128;128m"
	reset     = "\033[0m"
)

// Quiet 为 true 时不输出颜色（管道/重定向时更友好）。
var Quiet = os.Getenv("NO_COLOR") != ""

// Out / Err 允许测试替换输出目标。
var (
	Out io.Writer = os.Stdout
	Err io.Writer = os.Stderr
)

func paint(color, msg string) string {
	if Quiet {
		return msg
	}
	return color + msg + reset
}

// OK 打印成功信息（😼 前缀）。
func OK(format string, args ...any) {
	fmt.Fprintln(Out, paint(colorOK, "😼 "+fmt.Sprintf(format, args...)))
}

// OKEmoji 用指定 emoji 打印成功信息。
func OKEmoji(emoji, format string, args ...any) {
	fmt.Fprintln(Out, paint(colorOK, emoji+" "+fmt.Sprintf(format, args...)))
}

// Fail 打印失败信息（😾 前缀），返回 false 便于 `return cli.Fail(...)`。
func Fail(format string, args ...any) bool {
	fmt.Fprintln(Err, paint(colorFail, "😾 "+fmt.Sprintf(format, args...)))
	return false
}

// FailEmoji 用指定 emoji 打印失败信息。
func FailEmoji(emoji, format string, args ...any) {
	fmt.Fprintln(Err, paint(colorFail, emoji+" "+fmt.Sprintf(format, args...)))
}

// Error 打印致命错误（📢 前缀）。与 bash 版的区别：不 exec shell，
// 而是让调用方 return，交给 cobra 决定退出码。
func Error(format string, args ...any) {
	fmt.Fprintln(Err, paint(colorErr, "📢 "+fmt.Sprintf(format, args...)))
}

// Dim 打印次要信息。
func Dim(format string, args ...any) {
	fmt.Fprintln(Out, paint(colorDim, fmt.Sprintf(format, args...)))
}

// Line 打印一行不带装饰的文本到 stdout。
func Line(format string, args ...any) {
	fmt.Fprintf(Out, format+"\n", args...)
}
