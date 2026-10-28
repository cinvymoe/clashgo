// Package sub 负责订阅的增删改查、下载（含 subconverter 兜底）与切换。
//
// 与 bash 版的差异：
//   - 下载与校验分离，校验失败不落盘（bash 版会留下 temp.yaml）
//   - 不依赖 bin/yq，profiles.yaml 由 state 包读写
//   - 切换订阅后的生效统一走 kernel.Apply（热重载优先）
package sub

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"clashgo/internal/env"
	"clashgo/internal/kernel"
	"clashgo/internal/netutil"
	"clashgo/internal/state"
)

// Manager 把 env、订阅清单和内核管理器拢在一起。
//
// Validate 与 Apply 两个字段是内核操作的注入口：生产环境它们指向
// kernel.Manager，测试里可以换成纯内存实现，不必真跑 mihomo 或 systemctl。
type Manager struct {
	Env      *env.Env
	Kernel   *kernel.Manager
	Client   *http.Client
	Validate func(ctx context.Context, path string) error
	Apply    func(ctx context.Context, data []byte) (bool, error)
}

// New 构造 Manager。Client 强制绕过系统代理：订阅下载走直连才有意义，
// 否则在「代理已开启」的状态下再次更新订阅会绕着自己转。
func New(e *env.Env, k *kernel.Manager) *Manager {
	m := &Manager{
		Env:    e,
		Kernel: k,
		Client: &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				Proxy:               nil,
				TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
				DialContext:         (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
				TLSHandshakeTimeout: 15 * time.Second,
			},
		},
	}
	m.Validate = k.Validate
	m.Apply = k.Apply
	return m
}

// Store 读取订阅清单。
func (m *Manager) Store() (*state.Store, state.Path, error) {
	p := state.Path{
		Meta:     m.Env.ProfilesMeta(),
		ProfileF: m.Env.ProfilePath,
	}
	s, err := state.Load(p)
	return s, p, err
}

// Download 把订阅内容下载到内存并校验。返回可用于内核的配置字节。
//
// 流程与 bash 版一致：先下原始订阅，内核校验不过才走 subconverter。
// 区别是「原始内容」不落盘，避免在 resources/ 里堆一堆 temp.yaml.raw。
func (m *Manager) Download(ctx context.Context, rawURL string, forceConvert bool) ([]byte, error) {
	if rawURL == "" {
		return nil, errors.New("订阅链接为空")
	}
	if forceConvert {
		return m.downloadConverted(ctx, rawURL)
	}
	data, err := m.downloadRaw(ctx, rawURL)
	if err != nil {
		return nil, fmt.Errorf("下载订阅失败: %w", err)
	}
	verr := m.validateBytes(ctx, data)
	if verr == nil {
		return data, nil
	}
	if isUnsupported(verr) {
		// 内核不认这些协议，转换也救不回来，直接报清楚
		return nil, verr
	}
	converted, cerr := m.downloadConverted(ctx, rawURL)
	if cerr != nil {
		return nil, fmt.Errorf("订阅校验失败（%v），转换也未成功: %w", verr, cerr)
	}
	return converted, nil
}

// isUnsupported 判断校验失败是否属于「内核不支持该协议」。
func isUnsupported(err error) bool {
	var ue *kernel.UnsupportedProxyError
	return errors.As(err, &ue)
}

// validateBytes 用内核校验配置字节（写临时文件后交给 mihomo -t）。
func (m *Manager) validateBytes(ctx context.Context, data []byte) error {
	tmp, err := os.CreateTemp("", "clashgo-validate-*.yaml")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	tmp.Close()
	if m.Validate != nil {
		return m.Validate(ctx, path)
	}
	return m.Kernel.Validate(ctx, path)
}

// downloadRaw 直连下载原始订阅。
func (m *Manager) downloadRaw(ctx context.Context, rawURL string) ([]byte, error) {
	// file:// 或本地路径：直接读，方便离线测试
	if local, ok := localPath(rawURL); ok {
		data, err := os.ReadFile(local)
		if err != nil {
			return nil, err
		}
		if len(data) == 0 {
			return nil, errors.New("订阅内容为空")
		}
		return data, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	if ua := m.Env.SubUA; ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	resp, err := m.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求订阅失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("订阅返回 HTTP %d", resp.StatusCode)
	}
	// 限制 32MB，防止订阅端返回无穷流把内存吃满
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("读取订阅内容失败: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("订阅内容为空")
	}
	return data, nil
}

// localPath 识别 file:// 前缀与已存在的绝对路径。
func localPath(rawURL string) (string, bool) {
	if p, ok := strings.CutPrefix(rawURL, "file://"); ok {
		return p, true
	}
	if strings.HasPrefix(rawURL, "/") {
		if _, err := os.Stat(rawURL); err == nil {
			return rawURL, true
		}
	}
	return "", false
}

// downloadConverted 通过 bin/subconverter 转换后下载。
// 子进程的启停逻辑与 bash 版一致：已有服务在跑就直接复用。
func (m *Manager) downloadConverted(ctx context.Context, rawURL string) ([]byte, error) {
	port, err := m.ensureConverter(ctx)
	if err != nil {
		return nil, err
	}
	base := fmt.Sprintf("http://127.0.0.1:%d/sub", port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base, nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	q.Set("target", "clash")
	q.Set("url", rawURL)
	req.URL.RawQuery = q.Encode()
	if ua := m.Env.SubUA; ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	// 先拿到转换后的真实地址（subconverter 会 302 到 /get 之类），
	// 再由下面的循环跟随重定向
	resp, err := m.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("订阅转换失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("订阅转换返回 HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("读取转换结果失败: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("转换结果为空")
	}
	return data, nil
}

// ensureConverter 确保 subconverter 在监听，返回端口。
func (m *Manager) ensureConverter(ctx context.Context) (int, error) {
	port, err := m.converterPort()
	if err != nil {
		return 0, err
	}
	if m.converterAlive(ctx, port) {
		return port, nil
	}
	bin := m.Env.SubconvPath()
	if _, err := os.Stat(bin); err != nil {
		return 0, fmt.Errorf("找不到 subconverter：%s", bin)
	}
	logFile, err := os.OpenFile(m.Env.SubconvLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, fmt.Errorf("打开转换日志失败: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(bin)
	cmd.Dir = m.Env.SubconvDir()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("启动 subconverter 失败: %w", err)
	}
	// 后台等待，避免留下僵尸进程；转换完由 StopConverter 主动结束
	go func() { _ = cmd.Wait() }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if m.converterAlive(ctx, port) {
			return port, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return 0, fmt.Errorf("订阅转换服务未启动，请检查日志：%s", m.Env.SubconvLog())
}

// StopConverter 结束转换服务（转换是一次性的，用完就关，与 bash 版一致）。
func (m *Manager) StopConverter() {
	cmd := exec.Command("pkill", "-9", "-f", m.Env.SubconvPath())
	_ = cmd.Run()
}

// converterPort 从 pref.yml 里读 server.port，默认 25500。
// 端口被占用时随机换一个并写回 pref.yml，避免与内核端口打架。
func (m *Manager) converterPort() (int, error) {
	port := 25500
	if data, err := os.ReadFile(m.Env.SubconvPref()); err == nil {
		if p := readServerPort(string(data)); p > 0 {
			port = p
		}
	}
	if !netutil.PortInUse(port) {
		return port, nil
	}
	free, err := netutil.FreePort()
	if err != nil {
		return 0, err
	}
	m.rewriteConverterPort(free)
	return free, nil
}

// readServerPort 从 pref.yml 的 server 段里取 port。
// subconverter 的 pref.yml 结构简单，不值得引入 YAML 依赖做整树解析。
func readServerPort(text string) int {
	lines := strings.Split(text, "\n")
	inServer := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "server:") {
			inServer = true
			continue
		}
		if inServer {
			if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && trimmed != "" {
				break // 出了 server 段
			}
			if v, ok := strings.CutPrefix(trimmed, "port:"); ok {
				if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
					return n
				}
			}
		}
	}
	return 0
}

// rewriteConverterPort 把新端口写回 pref.yml（原地替换 server 段里的 port 行）。
func (m *Manager) rewriteConverterPort(port int) {
	path := m.Env.SubconvPref()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	inServer := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "server:") {
			inServer = true
			continue
		}
		if inServer {
			if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && trimmed != "" {
				break
			}
			if strings.HasPrefix(trimmed, "port:") {
				indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
				lines[i] = indent + "port: " + strconv.Itoa(port)
				break
			}
		}
	}
	_ = os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}

func (m *Manager) converterAlive(ctx context.Context, port int) bool {
	c := &http.Client{
		Timeout: 800 * time.Millisecond,
		Transport: &http.Transport{
			Proxy: nil,
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/version", port), nil)
	if err != nil {
		return false
	}
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// Add 添加订阅：下载 → 校验 → 落盘 profile 文件 → 写清单。
func (m *Manager) Add(ctx context.Context, rawURL string, forceConvert bool) (state.Profile, error) {
	store, path, err := m.Store()
	if err != nil {
		return state.Profile{}, err
	}
	data, err := m.Download(ctx, rawURL, forceConvert)
	if err != nil {
		return state.Profile{}, err
	}
	p, err := store.Add(rawURL)
	if err != nil {
		return state.Profile{}, err
	}
	if err := os.MkdirAll(m.Env.ProfilesDir(), 0o755); err != nil {
		return state.Profile{}, fmt.Errorf("创建订阅目录失败: %w", err)
	}
	if err := writeFileAtomic(m.Env.ProfilePath(p.ID), data); err != nil {
		return state.Profile{}, err
	}
	if err := store.Save(path); err != nil {
		return state.Profile{}, err
	}
	return p, nil
}

// Update 更新指定订阅（0 表示当前使用的订阅）。
// 返回是否同时切换了正在使用的配置。
func (m *Manager) Update(ctx context.Context, id int, forceConvert bool) (switched bool, err error) {
	store, _, err := m.Store()
	if err != nil {
		return false, err
	}
	if id == 0 {
		id = store.Use
		if id == 0 {
			return false, errors.New("当前没有正在使用的订阅，请指定 id")
		}
	}
	p := store.Find(id)
	if p == nil {
		return false, fmt.Errorf("订阅 id %d 不存在", id)
	}
	data, err := m.Download(ctx, p.URL, forceConvert)
	if err != nil {
		m.Log("❌ 订阅更新失败：[%d] %s", id, p.URL)
		return false, err
	}
	if err := writeFileAtomic(m.Env.ProfilePath(id), data); err != nil {
		return false, err
	}
	m.Log("✅ 订阅更新成功：[%d] %s", id, p.URL)

	// 更新的是当前订阅时，立即生效（热重载）
	if store.Use == id {
		if err := m.ApplyConfig(ctx, data); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// Use 切换当前使用的订阅，并热重载生效。
func (m *Manager) Use(ctx context.Context, id int) error {
	store, path, err := m.Store()
	if err != nil {
		return err
	}
	if len(store.Profiles) == 0 {
		return errors.New("当前无可用订阅，请先添加订阅")
	}
	if id == 0 {
		id = store.Use
	}
	p := store.Find(id)
	if p == nil {
		return fmt.Errorf("订阅 id %d 不存在", id)
	}
	data, err := os.ReadFile(m.Env.ProfilePath(id))
	if err != nil {
		return fmt.Errorf("读取订阅文件失败: %w", err)
	}
	if err := m.ApplyConfig(ctx, data); err != nil {
		return err
	}
	store.Use = id
	if err := store.Save(path); err != nil {
		return err
	}
	m.Log("🔥 订阅已切换为：[%d] %s", id, p.URL)
	return nil
}

// ApplyConfig 把订阅内容写成 config.yaml，合并 mixin 后交给内核生效。
func (m *Manager) ApplyConfig(ctx context.Context, profileData []byte) error {
	if err := writeFileAtomic(m.Env.ConfigBase(), profileData); err != nil {
		return err
	}
	merged, err := MergeConfig(m.Env)
	if err != nil {
		return err
	}
	if m.Apply != nil {
		_, err = m.Apply(ctx, merged)
		return err
	}
	_, err = m.Kernel.Apply(ctx, merged)
	return err
}

// Delete 删除订阅（正在使用的不允许删）。
func (m *Manager) Delete(id int) (state.Profile, error) {
	store, path, err := m.Store()
	if err != nil {
		return state.Profile{}, err
	}
	p, err := store.Remove(id)
	if err != nil {
		return state.Profile{}, err
	}
	_ = os.Remove(m.Env.ProfilePath(id))
	if err := store.Save(path); err != nil {
		return state.Profile{}, err
	}
	m.Log("➖ 已删除订阅：[%d] %s", id, p.URL)
	return p, nil
}

// Log 往 profiles.log 追加一行（格式与 bash 版一致）。
func (m *Manager) Log(format string, args ...any) {
	line := fmt.Sprintf("%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
	f, err := os.OpenFile(m.Env.ProfilesLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}

// writeFileAtomic 先写 .new 再 rename，避免下载中断留下半截配置。
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换 %s 失败: %w", path, err)
	}
	return nil
}
