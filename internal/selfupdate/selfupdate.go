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

// Repo 是 clashgo 的默认发布仓库。
// 可用环境变量 CLASHGO_REPO 或 Options.Repo 覆盖（fork / 私有分发用）。
var Repo = "cinvymoe/clashgo"

// RepoEnv 是覆盖发布仓库的环境变量名。
const RepoEnv = "CLASHGO_REPO"

// ChecksumAsset 是发布里的校验和文件名。
const ChecksumAsset = "checksums.txt"

// apiBase 单独抽出来，测试时可以指向 httptest。
// 可用环境变量 CLASHGO_API_BASE 覆盖（GitHub Enterprise / 镜像站）。
var apiBase = "https://api.github.com"

// APIBaseEnv 是覆盖 API 根地址的环境变量名。
const APIBaseEnv = "CLASHGO_API_BASE"

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
	// Repo 覆盖发布仓库，形如 owner/name；空则用包变量 Repo（可被 CLASHGO_REPO 覆盖）。
	Repo string
}

// repo 决定本次使用哪个发布仓库：Options > 环境变量 > 包默认值。
func (o Options) repo() string {
	if o.Repo != "" {
		return o.Repo
	}
	if v := os.Getenv(RepoEnv); v != "" {
		return v
	}
	return Repo
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
	// Verified 表示已与发布里的 checksums.txt 核对通过。
	Verified bool
}

// releaseChecksum 在发布产物里找 checksums.txt，取出 name 对应的 sha256。
// 找不到 checksums.txt 时返回 ok=false，由调用方决定是否接受（当前接受，但会提示）。
func releaseChecksum(ctx context.Context, rel *Release, name string) (string, bool, error) {
	var asset *Asset
	for i := range rel.Assets {
		if rel.Assets[i].Name == ChecksumAsset {
			asset = &rel.Assets[i]
			break
		}
	}
	if asset == nil {
		return "", false, nil
	}
	raw, err := download(ctx, *asset)
	if err != nil {
		return "", false, fmt.Errorf("下载校验和失败: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) != 2 {
			continue
		}
		// 标准格式：<sha256>  <filename>；
		// 兼容 sha256sum -b 的 * 前缀，以及 `sha256sum ./*.tar.gz` 产生的 ./ 前缀
		if strings.TrimPrefix(strings.TrimPrefix(f[1], "*"), "./") == name {
			if len(f[0]) != 64 {
				return "", false, fmt.Errorf("checksums.txt 里 %s 的校验和长度不对", name)
			}
			return f[0], true, nil
		}
	}
	return "", false, fmt.Errorf("checksums.txt 里没有 %s 的记录", name)
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
	if v := os.Getenv(APIBaseEnv); v != "" {
		return strings.TrimRight(v, "/")
	}
	return apiBase
}

// AssetName 返回当前平台对应的产物名，形如 clashgo-linux-amd64.tar.gz。
func AssetName() string {
	return fmt.Sprintf("clashgo-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
}

// FetchRelease 查最新（或指定 tag）的发布。
func FetchRelease(ctx context.Context, o Options) (*Release, error) {
	repo := o.repo()
	u := o.base() + "/repos/" + repo + "/releases/latest"
	if o.Tag != "" {
		u = o.base() + "/repos/" + repo + "/releases/tags/" + o.Tag
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
			return nil, fmt.Errorf("找不到版本 %s（仓库 %s 可能还没发布过）", o.Tag, repo)
		}
		return nil, fmt.Errorf("仓库 %s 还没有任何发布", repo)
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
	// 发布里的 checksums.txt 记的是「产物文件」的哈希（tar.gz 本身），
	// 所以要按下载到的原始字节核对；binSum 只用于兼容「发布裸二进制哈希」的项目。
	rawSum := sha256.Sum256(raw)
	binSum := sha256.Sum256(bin)
	res.SHA256 = hex.EncodeToString(rawSum[:])
	res.Downloaded = int64(len(raw))

	// 校验和：发布里带 checksums.txt 就强制核对。
	// 只打印 sha256 而不比对，等于没有完整性校验——TLS 只能证明「连的是 GitHub」，
	// 不能证明「产物就是你发布的那一份」。
	if asset.Name != "" {
		if want, ok, cerr := releaseChecksum(ctx, rel, asset.Name); cerr != nil {
			return res, cerr
		} else if ok {
			if !strings.EqualFold(want, res.SHA256) && !strings.EqualFold(want, hex.EncodeToString(binSum[:])) {
				return res, fmt.Errorf("校验和不匹配：发布声明 %s，实际下载 %s", want[:12], res.SHA256[:12])
			}
			res.Verified = true
		}
	}

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
