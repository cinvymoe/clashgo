package cli

import (
	"fmt"
	"io"
	"os"
)

// Terminal 把输出目标封装起来，便于测试捕获与未来 daemon 复用到日志。
//
// 与包级函数（OK/Fail/...）的关系：包级函数写 os.Stdout/os.Stderr，
// Terminal 允许指定任意 writer。app 层统一走 Terminal。
type Terminal struct {
	Out   io.Writer
	Err   io.Writer
	Quiet bool
}

// IsTTY 判断 writer 是否连在终端上（重定向到文件时不加颜色）。
func IsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// NewTerminal 返回写到 os.Stdout/os.Stderr 的终端。
func NewTerminal() *Terminal {
	return &Terminal{Out: os.Stdout, Err: os.Stderr, Quiet: Quiet || !IsTTY(os.Stdout)}
}

// NewBuffer 返回写到指定 writer 的终端（测试用，默认无颜色）。
func NewBuffer(out, errw io.Writer) *Terminal {
	return &Terminal{Out: out, Err: errw, Quiet: true}
}

func (t *Terminal) colorize(color, msg string) string {
	if t.Quiet || color == "" {
		return msg
	}
	return color + msg + reset
}

// OK 打印成功信息。
func (t *Terminal) OK(format string, args ...any) {
	fmt.Fprintln(t.Out, t.colorize(colorOK, "😼 "+fmt.Sprintf(format, args...)))
}

// OKEmoji 用指定 emoji 打印成功信息。
func (t *Terminal) OKEmoji(emoji, format string, args ...any) {
	fmt.Fprintln(t.Out, t.colorize(colorOK, emoji+" "+fmt.Sprintf(format, args...)))
}

// Fail 打印失败信息并返回 false，便于 `return t.Fail(...)`。
func (t *Terminal) Fail(format string, args ...any) bool {
	fmt.Fprintln(t.Err, t.colorize(colorFail, "😾 "+fmt.Sprintf(format, args...)))
	return false
}

// FailEmoji 用指定 emoji 打印失败信息。
func (t *Terminal) FailEmoji(emoji, format string, args ...any) {
	fmt.Fprintln(t.Err, t.colorize(colorFail, emoji+" "+fmt.Sprintf(format, args...)))
}

// Error 打印致命错误。
func (t *Terminal) Error(format string, args ...any) {
	fmt.Fprintln(t.Err, t.colorize(colorErr, "📢 "+fmt.Sprintf(format, args...)))
}

// Dim 打印次要信息。
func (t *Terminal) Dim(format string, args ...any) {
	fmt.Fprintln(t.Out, t.colorize(colorDim, fmt.Sprintf(format, args...)))
}

// Line 打印无装饰的一行。
func (t *Terminal) Line(format string, args ...any) {
	fmt.Fprintf(t.Out, format+"\n", args...)
}
