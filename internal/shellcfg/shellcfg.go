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

// RcFiles 返回需要维护托管块的 rc 文件（存在的才会返回）。
func RcFiles() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	var out []string
	for _, name := range []string{".bashrc", ".zshrc"} {
		p := filepath.Join(home, name)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// block 生成写在 rc 里的托管块。
func block() string {
	return BlockStart + "\n" +
		`[ -f "$HOME/.config/clashgo/proxy.env" ] && . "$HOME/.config/clashgo/proxy.env"` + "\n" +
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
	rest, had := stripBlock(string(data))
	if had && strings.Contains(string(data), block()) {
		return false, nil // 已经是正确形态，无需重写
	}
	if rest != "" && !strings.HasSuffix(rest, "\n") {
		rest += "\n"
	}
	out := rest + block()
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
