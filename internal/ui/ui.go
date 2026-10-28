// Package ui 管理 zashboard 前端（resources/dist）的安装与更新。
//
// mihomo 的 `external-ui: dist` 让它把 resources/dist 当静态站点服务，
// 所以「更新面板」= 下载 dist.zip → 校验 → 原子替换整个目录。
package ui

import (
	"archive/zip"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"clashgo/internal/env"
)

// DefaultReleaseURL 是 zashboard 的官方最新 release 包。
const DefaultReleaseURL = "https://github.com/Zephyruso/zashboard/releases/latest/download/dist.zip"

// Manager 负责下载与安装面板。
type Manager struct {
	Env    *env.Env
	Client *http.Client
	URL    string // 允许覆盖下载地址（测试/镜像）
}

// New 构造 Manager。下载不走代理：更新面板时若代理本身已经挂了，
// 走代理只会死循环。
func New(e *env.Env) *Manager {
	return &Manager{
		Env: e,
		URL: DefaultReleaseURL,
		Client: &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				Proxy:           nil,
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}
}

// Installed 报告 resources/dist 是否已经装过面板。
func (m *Manager) Installed() bool {
	_, err := os.Stat(filepath.Join(m.Env.DistDir(), "index.html"))
	return err == nil
}

// IndexSize 返回已安装面板的 index.html 大小，用作粗略的健康检查。
func (m *Manager) IndexSize() int64 {
	st, err := os.Stat(filepath.Join(m.Env.DistDir(), "index.html"))
	if err != nil {
		return 0
	}
	return st.Size()
}

// fetch 下载 zip 内容。
func (m *Manager) fetch() ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, m.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	resp, err := m.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载面板失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("下载面板返回 HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 128<<20))
	if err != nil {
		return nil, fmt.Errorf("读取面板数据失败: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("面板数据为空")
	}
	return data, nil
}

// Install 下载最新面板并原子替换 resources/dist。
//
// 原子性做法：先解压到 dist.new，确认有 index.html 后再把旧目录改名成
// dist.old、新目录改名成 dist、最后删掉 dist.old。任一步失败都不会让
// dist 处于「既有旧文件又有新文件」的半成品状态。
func (m *Manager) Install() (files int, err error) {
	data, err := m.fetch()
	if err != nil {
		return 0, err
	}
	return m.InstallFromBytes(data)
}

// InstallFromBytes 用给定的 zip 内容安装（便于测试与离线安装）。
func (m *Manager) InstallFromBytes(data []byte) (files int, err error) {
	if len(data) == 0 {
		return 0, errors.New("面板数据为空")
	}
	dist := m.Env.DistDir()
	staging := dist + ".new"
	// 清理上一次可能残留的临时目录
	if err := os.RemoveAll(staging); err != nil {
		return 0, fmt.Errorf("清理 %s 失败: %w", staging, err)
	}
	files, err = extractZip(data, staging)
	if err != nil {
		_ = os.RemoveAll(staging)
		return 0, err
	}
	if _, err := os.Stat(filepath.Join(staging, "index.html")); err != nil {
		_ = os.RemoveAll(staging)
		return 0, errors.New("面板包里没有 index.html，可能不是有效的前端包")
	}
	backup := dist + ".old"
	_ = os.RemoveAll(backup)
	if _, err := os.Stat(dist); err == nil {
		if err := os.Rename(dist, backup); err != nil {
			_ = os.RemoveAll(staging)
			return 0, fmt.Errorf("备份旧面板失败: %w", err)
		}
	}
	if err := os.Rename(staging, dist); err != nil {
		// 回滚，保证 dist 仍然是旧的那一份
		_ = os.Rename(backup, dist)
		_ = os.RemoveAll(staging)
		return 0, fmt.Errorf("安装新面板失败: %w", err)
	}
	_ = os.RemoveAll(backup)
	return files, nil
}

// extractZip 把 zip 解到目标目录，并剥掉顶层的 dist/ 前缀。
//
// zashboard 的 release 包结构是 `dist/index.html`，而 mihomo 期望
// `resources/dist/index.html`，所以这里的顶层目录必须被移除。
func extractZip(data []byte, dest string) (int, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return 0, fmt.Errorf("解析 zip 失败: %w", err)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return 0, fmt.Errorf("创建目录失败: %w", err)
	}
	// 判断是否需要剥掉顶层目录：所有条目都在同一个顶层目录下时才剥
	strip := commonTopDir(zr)

	count := 0
	for _, f := range zr.File {
		name := f.Name
		if strip != "" {
			name = strings.TrimPrefix(name, strip+"/")
			if name == "" {
				continue
			}
		}
		// 防目录穿越
		clean := path.Clean("/" + name)
		if strings.HasPrefix(clean, "/../") {
			return 0, fmt.Errorf("zip 内含非法路径: %s", f.Name)
		}
		target := filepath.Join(dest, filepath.FromSlash(strings.TrimPrefix(clean, "/")))

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return 0, fmt.Errorf("创建目录 %s 失败: %w", target, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return 0, fmt.Errorf("创建目录失败: %w", err)
		}
		rc, err := f.Open()
		if err != nil {
			return 0, fmt.Errorf("打开 %s 失败: %w", f.Name, err)
		}
		out, err := os.Create(target)
		if err != nil {
			rc.Close()
			return 0, fmt.Errorf("创建 %s 失败: %w", target, err)
		}
		_, cerr := io.Copy(out, rc) //nolint:gosec // zip 内容来自可信 release
		out.Close()
		rc.Close()
		if cerr != nil {
			return 0, fmt.Errorf("写入 %s 失败: %w", target, cerr)
		}
		count++
	}
	if count == 0 {
		return 0, errors.New("面板包内没有任何文件")
	}
	return count, nil
}

// commonTopDir 返回所有条目共有的顶层目录名；没有共同顶层目录时返回空串。
func commonTopDir(zr *zip.Reader) string {
	top := ""
	for _, f := range zr.File {
		name := strings.TrimPrefix(f.Name, "./")
		if name == "" {
			continue
		}
		i := strings.Index(name, "/")
		if i < 0 {
			return "" // 有文件直接躺在根上，不剥
		}
		dir := name[:i]
		if top == "" {
			top = dir
		} else if top != dir {
			return ""
		}
	}
	if top == "index.html" {
		return ""
	}
	return top
}

// APIURL 拼出面板地址，供 clashgo ui 打印。
func (m *Manager) APIURL(host string, port int) string {
	return fmt.Sprintf("http://%s:%d/ui", host, port)
}
