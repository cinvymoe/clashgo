// Package shellcfg 负责「让 shell 知道代理已开启」这件事。
//
// Go 进程无法修改父 shell 的环境变量，所以这里不引入 shell 函数壳子，
// 而是直接写配置：把 export 语句落到 ~/.config/clashgo/proxy.env，
// 再在 ~/.bashrc / ~/.zshrc 里维护一个托管块 source 它。
//
// 代价写在命令输出里：改动不会影响当前已经打开的 shell，需要 source 或新开窗口。
package shellcfg

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// BlockStart / BlockEnd 是 rc 文件里托管块的边界标记。
	BlockStart = "# clashgo START"
	BlockEnd   = "# clashgo END"

	// defaultNoProxy 与 bash 版保持一致。
	defaultNoProxy = "localhost,127.0.0.1,::1"
)

// Proxy 描述一次「开启系统代理」需要写进 shell 的全部信息。
type Proxy struct {
	HTTPPort    int    // http_port，缺省回退到 MixedPort
	SocksPort   int    // socks_port，缺省回退到 MixedPort
	MixedPort   int    // 兜底端口
	BindAddress string // 缺省 127.0.0.1
	Auth        string // "user:pass"，为空表示无需认证
	NoProxy     string // 缺省 defaultNoProxy
}

// ConfigDir 返回 clashgo 自己的配置目录。
func ConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("无法确定用户主目录: %w", err)
	}
	return filepath.Join(home, ".config", "clashgo"), nil
}

// ProxyFile 返回 proxy.env 的完整路径。
func ProxyFile() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "proxy.env"), nil
}

// Render 生成 proxy.env 的内容（全部是 POSIX sh 兼容的 export 语句）。
func (p Proxy) Render() string {
	httpPort := p.HTTPPort
	if httpPort == 0 {
		httpPort = p.MixedPort
	}
	socksPort := p.SocksPort
	if socksPort == 0 {
		socksPort = p.MixedPort
	}
	bind := p.BindAddress
	if bind == "" || bind == "*" {
		bind = "127.0.0.1"
	}
	auth := ""
	if p.Auth != "" {
		auth = p.Auth + "@"
	}
	noProxy := p.NoProxy
	if noProxy == "" {
		noProxy = defaultNoProxy
	}

	httpAddr := fmt.Sprintf("http://%s%s:%d", auth, bind, httpPort)
	socksAddr := fmt.Sprintf("socks5h://%s%s:%d", auth, bind, socksPort)

	var b strings.Builder
	b.WriteString("# 由 clashgo 生成，请勿手工编辑。clashgo off 会删除本文件。\n")
	fmt.Fprintf(&b, "export http_proxy=%s\n", httpAddr)
	b.WriteString("export HTTP_PROXY=$http_proxy\n")
	fmt.Fprintf(&b, "export https_proxy=%s\n", httpAddr)
	b.WriteString("export HTTPS_PROXY=$https_proxy\n")
	fmt.Fprintf(&b, "export all_proxy=%s\n", socksAddr)
	b.WriteString("export ALL_PROXY=$all_proxy\n")
	fmt.Fprintf(&b, "export no_proxy=%s\n", noProxy)
	b.WriteString("export NO_PROXY=$no_proxy\n")
	return b.String()
}

// Write 把内容原子写入 proxy.env。
func Write(content string) (string, error) {
	path, err := ProxyFile()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("创建 %s 失败: %w", filepath.Dir(path), err)
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("写入 %s 失败: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("替换 %s 失败: %w", path, err)
	}
	return path, nil
}

// Remove 删除 proxy.env。文件本来就不存在时不算错误（幂等）。
func Remove() error {
	path, err := ProxyFile()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除 %s 失败: %w", path, err)
	}
	return nil
}

// rcCandidates 是各 shell 会读取的启动文件。只覆盖真正存在的文件，
// 避免凭空创建 .zshrc 之类用户并不用的配置。
//
// 顺序按「交互 shell → 登录 shell → POSIX 通用」排列：
//   - .bashrc / .zshrc：交互式非登录 shell（终端里手敲 bash/zsh）
//   - .bash_profile / .zprofile：登录 shell（SSH 登录、bash -l）。
//     .bash_profile 存在时 bash 不会读 .profile，所以要单独照顾。
//   - .profile：POSIX 通用入口，dash/sh 与「没有 .bash_profile 的登录 bash」走这条
//   - .zshrc 之外 zsh 登录还会读 .zprofile，同理
//   - config.fish：fish 语法不同，单独处理（见 fishBlock）
var rcCandidates = []string{
	".bashrc",
	".zshrc",
	".bash_profile",
	".zprofile",
	".profile",
}

// RcFiles 返回需要维护托管块的启动文件（存在的才会返回）。
func RcFiles() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	var out []string
	for _, name := range rcCandidates {
		p := filepath.Join(home, name)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	// fish 只有在真正安装过、且配置文件已存在时才处理
	if p := fishConfig(); p != "" {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// fishConfig 返回 fish 的配置文件路径（不存在也返回路径，由调用方判断）。
func fishConfig() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "fish", "config.fish")
	}
	return filepath.Join(home, ".config", "fish", "config.fish")
}

// isFish 判断该文件是不是 fish 配置。
func isFish(path string) bool {
	return filepath.Base(path) == "config.fish"
}

// block 生成写在 rc 里的托管块（POSIX shell 语法）。
//
// 关键点：不只是 source 一次，而是注册一个「每个提示符前检查一次」的钩子。
// 这样另一个终端里跑了 clashgo on/off 之后，已经开着的这个终端在下次回车时
// 就会自动跟上（外部进程无法修改已运行 shell 的环境变量，只能靠 shell 自己重新加载）。
//
// 判定方式：把 proxy.env 里的 http_proxy 期望值与当前值直接比对，
// 全程只用 shell 内建（read / case / unset），不 fork 任何进程，
// 也不用全局时间戳文件（多个 shell 共享同一时间戳会互相干扰）。
func block() string {
	return BlockStart + "\n" +
		`__clashgo_env="$HOME/.config/clashgo/proxy.env"` + "\n" +
		`__clashgo_refresh() {` + "\n" +
		`    if [ -f "$__clashgo_env" ]; then` + "\n" +
		`        __clashgo_line=""` + "\n" +
		`        while IFS= read -r __clashgo_l; do` + "\n" +
		`            case $__clashgo_l in export\ http_proxy=*) __clashgo_line=$__clashgo_l; break ;; esac` + "\n" +
		`        done <"$__clashgo_env"` + "\n" +
		`        __clashgo_want=${__clashgo_line#export http_proxy=}` + "\n" +
		`        if [ -n "$__clashgo_want" ] && [ "$__clashgo_want" = "${http_proxy:-}" ]; then` + "\n" +
		`            __clashgo_on=1` + "\n" +
		`            return 0` + "\n" +
		`        fi` + "\n" +
		`        . "$__clashgo_env"` + "\n" +
		`        __clashgo_on=1` + "\n" +
		`    else` + "\n" +
		`        [ -n "${__clashgo_on:-}" ] || return 0` + "\n" +
		`        unset http_proxy HTTP_PROXY https_proxy HTTPS_PROXY all_proxy ALL_PROXY no_proxy NO_PROXY` + "\n" +
		`        unset __clashgo_on` + "\n" +
		`    fi` + "\n" +
		`}` + "\n" +
		`__clashgo_refresh` + "\n" +
		`if [ -n "${BASH_VERSION:-}" ]; then` + "\n" +
		`    case ";${PROMPT_COMMAND:-};" in` + "\n" +
		`        *";__clashgo_refresh;"*) ;;` + "\n" +
		`        *) PROMPT_COMMAND="__clashgo_refresh${PROMPT_COMMAND:+;$PROMPT_COMMAND}" ;;` + "\n" +
		`    esac` + "\n" +
		`elif [ -n "${ZSH_VERSION:-}" ]; then` + "\n" +
		`    autoload -Uz add-zsh-hook 2>/dev/null && add-zsh-hook precmd __clashgo_refresh 2>/dev/null` + "\n" +
		`fi` + "\n" +
		BlockEnd + "\n"
}

// fishBlock 生成 fish 的托管块。fish 不能 source POSIX 的 export 语法，
// 因此逐行转成 set -gx；同时用 fish_prompt 事件实现同样的「每次提示符前检查」。
func fishBlock() string {
	return BlockStart + "\n" +
		`set -g _clashgo_env "$HOME/.config/clashgo/proxy.env"` + "\n" +
		`function __clashgo_refresh --on-event fish_prompt` + "\n" +
		`    if test -f "$_clashgo_env"` + "\n" +
		`        set -l _clashgo_want (string replace -r '^export http_proxy=' '' -- (string match -r '^export http_proxy=.*' -- <"$_clashgo_env"))` + "\n" +
		`        if test -n "$_clashgo_want"; and test "$_clashgo_want" = "$http_proxy"` + "\n" +
		`            set -g _clashgo_on 1` + "\n" +
		`            return` + "\n" +
		`        end` + "\n" +
		`        for _clashgo_line in (string split \n <"$_clashgo_env")` + "\n" +
		`            if string match -qr '^export ' -- $_clashgo_line` + "\n" +
		`                set -l _clashgo_kv (string replace -r '^export ' '' -- $_clashgo_line | string split -m1 '=')` + "\n" +
		`                set -gx $_clashgo_kv[1] $_clashgo_kv[2]` + "\n" +
		`            end` + "\n" +
		`        end` + "\n" +
		`        set -g _clashgo_on 1` + "\n" +
		`    else` + "\n" +
		`        test -n "$_clashgo_on"; or return` + "\n" +
		`        for _clashgo_n in http_proxy HTTP_PROXY https_proxy HTTPS_PROXY all_proxy ALL_PROXY no_proxy NO_PROXY` + "\n" +
		`            set -e $_clashgo_n` + "\n" +
		`        end` + "\n" +
		`        set -e _clashgo_on` + "\n" +
		`    end` + "\n" +
		`end` + "\n" +
		`__clashgo_refresh` + "\n" +
		BlockEnd + "\n"
}

// stripBlock 去掉已有的托管块，返回剩余内容与「是否原本存在」。
func stripBlock(content string) (string, bool) {
	start := strings.Index(content, BlockStart)
	if start < 0 {
		return content, false
	}
	end := strings.Index(content[start:], BlockEnd)
	if end < 0 {
		// 有头无尾，说明文件被手工改坏了，只切掉到文件末尾
		return content[:start], true
	}
	end += start + len(BlockEnd)
	// 连同紧随其后的换行一起吃掉
	for end < len(content) && (content[end] == '\n' || content[end] == '\r') {
		end++
	}
	return content[:start] + content[end:], true
}

// EnsureBlock 在 rc 文件末尾维护 source 托管块。已存在且内容正确时不会改动文件。
// 返回是否真的写了文件。
func EnsureBlock(rcPath string) (bool, error) {
	data, err := os.ReadFile(rcPath)
	if err != nil {
		return false, fmt.Errorf("读取 %s 失败: %w", rcPath, err)
	}
	want := block()
	if isFish(rcPath) {
		want = fishBlock()
	}
	rest, had := stripBlock(string(data))
	if had && strings.Contains(string(data), want) {
		return false, nil // 已经是正确形态，无需重写
	}
	if rest != "" && !strings.HasSuffix(rest, "\n") {
		rest += "\n"
	}
	out := rest + want
	if err := os.WriteFile(rcPath, []byte(out), 0o644); err != nil {
		return false, fmt.Errorf("写入 %s 失败: %w", rcPath, err)
	}
	return true, nil
}

// RevokeBlock 移除 rc 文件里的托管块。块不存在时视为成功（幂等）。
func RevokeBlock(rcPath string) (bool, error) {
	data, err := os.ReadFile(rcPath)
	if err != nil {
		return false, fmt.Errorf("读取 %s 失败: %w", rcPath, err)
	}
	rest, had := stripBlock(string(data))
	if !had {
		return false, nil
	}
	if err := os.WriteFile(rcPath, []byte(rest), 0o644); err != nil {
		return false, fmt.Errorf("写入 %s 失败: %w", rcPath, err)
	}
	return true, nil
}
