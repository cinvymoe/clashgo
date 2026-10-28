// Package selfupdate 负责 clashgo 自身的升级：从 GitHub Releases 拉最新二进制，
// 原子替换当前可执行文件。
//
// 关键安全点：
//   - 新二进制先落到同目录的临时文件，不直接覆盖在跑的文件
//     （Linux 上直接写正在执行的 inode 会 ETXTBSY；os.Rename 是目录项替换，安全）
//   - 替换前用 `version --short` 跑一次自检，坏包/错架构不会把能用的版本搞掉
//   - 替换失败自动回滚到备份
package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Repo 是 clashgo 的发布仓库。首次发布前需要先在 GitHub 建仓库并打 tag。
const Repo = "cinvymoe/clashgo"

// apiBase 单独抽出来，测试时可以指向 httptest。
var apiBase = "https://api.github.com"

// Proxy 允许覆盖下载代理；默认直连（GitHub API 走代理常被 403 限流）。
var Proxy func(*http.Request) (*url.URL, error)

// Release 是 GitHub Releases API 的裁剪视图。
type Release struct {
	TagName    string  `json:"tag_name"`
	Name       string  `json:"name"`
	Prerelease bool    `json:"prerelease"`
	Draft      bool    `json:"draft"`
	Assets     []Asset `json:"assets"`
}

// Asset 是一个发布产物。
type Asset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"browser_download_url"`
}

// Options 控制升级行为。
type Options struct {
	Tag      string // 指定版本（如 v0.2.0），空则取最新
	Pre      bool   // 是否接受预发布版本
	BaseURL  string // 覆盖 apiBase（测试用）
	DryRun   bool   // 只检查不替换
	NoVerify bool   // 跳过自检（不建议）
}

// Result 描述升级结果。
type Result struct {
	From       string
	To         string
	Asset      string
	Downloaded int64
	SHA256     string
	Applied    bool
	UpToDate   bool
}

func httpClient() *http.Client {
	tr := &http.Transport{
		Proxy:                 Proxy,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	return &http.Client{Transport: tr, Timeout: 5 * time.Minute}
}

func (o Options) base() string {
	if o.BaseURL != "" {
		return strings.TrimRight(o.BaseURL, "/")
	}
	return apiBase
}

// AssetName 返回当前平台对应的产物名，形如 clashgo-linux-amd64.tar.gz。
func AssetName() string {
	return fmt.Sprintf("clashgo-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
}

// FetchRelease 查最新（或指定 tag）的发布。
func FetchRelease(ctx context.Context, o Options) (*Release, error) {
	u := o.base() + "/repos/" + Repo + "/releases/latest"
	if o.Tag != "" {
		u = o.base() + "/repos/" + Repo + "/releases/tags/" + o.Tag
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "clashgo-selfupdate")
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("查询发布信息失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		if o.Tag != "" {
			return nil, fmt.Errorf("找不到版本 %s（仓库 %s 可能还没发布过）", o.Tag, Repo)
		}
		return nil, fmt.Errorf("仓库 %s 还没有任何发布", Repo)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("查询发布信息失败：HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var r Release
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("解析发布信息失败: %w", err)
	}
	if r.Draft {
		return nil, fmt.Errorf("最新发布是草稿，跳过")
	}
	if r.Prerelease && !o.Pre {
		return nil, fmt.Errorf("最新发布是预发布版本（%s），加 --pre 才会安装", r.TagName)
	}
	return &r, nil
}

// PickAsset 在发布里找当前平台的产物。
func (r *Release) PickAsset() (Asset, error) {
	want := AssetName()
	for _, a := range r.Assets {
		if a.Name == want {
			return a, nil
		}
	}
	// 退一步：允许裸二进制（不带 tar.gz）
	plain := strings.TrimSuffix(want, ".tar.gz")
	for _, a := range r.Assets {
		if a.Name == plain {
			return a, nil
		}
	}
	var have []string
	for _, a := range r.Assets {
		have = append(have, a.Name)
	}
	return Asset{}, fmt.Errorf("发布 %s 里没有 %s（现有：%s）", r.TagName, want, strings.Join(have, ", "))
}

// download 把产物读进内存。clashgo 二进制约 10MB，内存里放着没问题，
// 而且比先落盘再解压少一次 IO。
func download(ctx context.Context, a Asset) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "clashgo-selfupdate")
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载 %s 失败: %w", a.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载 %s 失败：HTTP %d", a.Name, resp.StatusCode)
	}
	// 上限 200MB，防止仓库被塞了异常大的产物
	data, err := io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	if err != nil {
		return nil, fmt.Errorf("下载 %s 失败: %w", a.Name, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("下载 %s 得到空内容", a.Name)
	}
	return data, nil
}

// ExtractBinary 从 tar.gz 里取出名为 clashgo 的文件；若是裸二进制则原样返回。
func ExtractBinary(data []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err == nil {
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("解析压缩包失败: %w", err)
			}
			if h.Typeflag != tar.TypeReg {
				continue
			}
			base := filepath.Base(h.Name)
			if base == "clashgo" || base == "clashgo.exe" {
				return io.ReadAll(io.LimitReader(tr, 200<<20))
			}
		}
		return nil, fmt.Errorf("压缩包里找不到 clashgo 可执行文件")
	}
	// 退一步当裸二进制
	return data, nil
}

// CurrentVersion 返回当前二进制的版本号（跑 `clashgo version --short`）。
func CurrentVersion(ctx context.Context, self string) string {
	cmd := exec.CommandContext(ctx, self, "version", "--short")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Update 执行升级。self 是当前可执行文件路径。
func Update(ctx context.Context, self string, o Options) (Result, error) {
	var res Result
	res.From = CurrentVersion(ctx, self)

	rel, err := FetchRelease(ctx, o)
	if err != nil {
		return res, err
	}
	res.To = rel.TagName
	if res.From != "" && NormalizeVersion(res.From) == NormalizeVersion(res.To) {
		res.UpToDate = true
		return res, nil
	}

	asset, err := rel.PickAsset()
	if err != nil {
		return res, err
	}
	res.Asset = asset.Name

	raw, err := download(ctx, asset)
	if err != nil {
		return res, err
	}
	bin, err := ExtractBinary(raw)
	if err != nil {
		return res, err
	}
	sum := sha256.Sum256(bin)
	res.SHA256 = hex.EncodeToString(sum[:])
	res.Downloaded = int64(len(bin))

	if o.DryRun {
		return res, nil
	}

	if err := Replace(self, bin, o.NoVerify); err != nil {
		return res, err
	}
	res.Applied = true
	return res, nil
}

// Replace 原子替换可执行文件。
//
// 顺序：写 .new → 自检 → 备份旧文件 → rename → 失败回滚。
func Replace(self string, bin []byte, noVerify bool) error {
	dir := filepath.Dir(self)
	tmp := filepath.Join(dir, ".clashgo-new")
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return fmt.Errorf("写入新版本失败（%s 可能不可写，需要 root）: %w", dir, err)
	}
	cleanup := func() { _ = os.Remove(tmp) }

	if !noVerify {
		cmd := exec.Command(tmp, "version", "--short")
		if out, err := cmd.CombinedOutput(); err != nil {
			cleanup()
			return fmt.Errorf("新版本自检失败，已保留原文件: %v（输出：%s）", err, strings.TrimSpace(string(out)))
		}
	}

	backup := self + ".bak"
	_ = os.Remove(backup)
	if err := os.Rename(self, backup); err != nil {
		cleanup()
		return fmt.Errorf("备份原文件失败: %w", err)
	}
	if err := os.Rename(tmp, self); err != nil {
		_ = os.Rename(backup, self)
		cleanup()
		return fmt.Errorf("替换失败，已回滚: %w", err)
	}
	// 软链（clashon 等）指向的是同一个路径，rename 后仍有效
	return nil
}

// NormalizeVersion 把 v0.2.0 / 0.2.0 / dev 统一成可比较的形式。
func NormalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	// 去掉 git describe 尾巴，如 0.2.0-3-gabc123 → 0.2.0
	if i := strings.Index(v, "-"); i > 0 {
		v = v[:i]
	}
	return v
}
