// Package install 实现 `clashgo install`：在一台干净机器上从零搭起整套环境。
//
// 对应 bash 版 scripts/preflight.sh 的全部职责，但做了两处收敛：
//   - 只支持 systemd（bash 版还有 SysVinit / OpenRC / runit / nohup 四个后端，
//     共 70 行纯 shell，维护成本远超收益）
//   - 不复制整个仓库到安装目录，只生成运行期真正需要的文件
package install

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"clashgo/internal/ui"
)

// Options 是安装参数。
type Options struct {
	BaseDir   string // 安装目录
	KernelURL string // 内核下载地址；空则按架构自动推断
	UIURL     string // zashboard dist.zip 地址；空则用默认
	Secret    string // Web 控制密钥；空则随机生成 6 位
	MixedPort int    // HTTP/SOCKS 混合端口，默认 7890
	APIPort   int    // external-controller 端口，默认 9090
	Force     bool   // 安装目录已存在时是否覆盖
	DryRun    bool   // 只打印计划不落盘
	// GhProxy 是 GitHub 下载加速前缀，形如 https://ghproxy.com。
	// 国内直连 GitHub release 经常失败（实测本机直连返回 000），
	// bash 版把这一项放在 .env 的 URL_GH_PROXY 里。
	GhProxy string
	// UnitDir 是 systemd unit 的落盘目录，默认 /etc/systemd/system。
	// 可用 CLASHGO_UNIT_DIR 环境变量覆盖（非标准部署与测试用）。
	UnitDir string
}

// DefaultUnitDir 是 systemd unit 的标准目录。
const DefaultUnitDir = "/etc/systemd/system"

// resolveUnitDir 决定 unit 目录。
func resolveUnitDir(o Options) string {
	if o.UnitDir != "" {
		return o.UnitDir
	}
	if v := os.Getenv("CLASHGO_UNIT_DIR"); v != "" {
		return v
	}
	return DefaultUnitDir
}

// Step 是一次可汇报的安装动作。
type Step struct {
	Name string
	Done bool
	Skip bool
	Note string
}

// Reporter 让调用方（app 层）决定怎么打印。
type Reporter interface {
	Step(name string)
	Skip(name, reason string)
	Detail(format string, args ...any)
}

// DefaultBaseDir 是默认安装目录，与 clashctl 的惯例一致。
const DefaultBaseDir = "/data/project/clashgo-instance"

// DefaultKernelVersion 是默认拉取的 mihomo 版本。
// 形如 v1.19.17；CPU 支持 AVX2 时会自动加 v3- 前缀。
const DefaultKernelVersion = "v1.19.33"

// DefaultUIURL 是 zashboard 的官方发布地址。
const DefaultUIURL = "https://github.com/Zephyruso/zashboard/releases/latest/download/dist.zip"

// DefaultMixin 是首次安装时生成的 mixin.yaml 模板。
// 内容与 bash 版安装后得到的一致（保留全部注释，用户能看懂每项为什么在）。
const DefaultMixin = `_custom:
  system-proxy:
    enable: true
mixed-port: 7890
external-controller: '0.0.0.0:9090'
external-ui: dist
external-ui-url: https://github.com/Zephyruso/zashboard/releases/latest/download/dist.zip
secret: __SECRET__
allow-lan: true # 若开启务必设置用户验证以防暴露公网后被滥用
#authentication:
#  - "admin:admin123"

# prefix：前置插入
# suffix：后置插入
# override：根据 name 匹配并替换原项
# inject (仅 proxy-groups)：按 name 匹配，把指定 proxy 追加到已有 group 的 .proxies 列表（自动去重）
rules:
  prefix:
    - DOMAIN,api64.ipify.org,DIRECT # 用于 clashui 获取真实公网 IP
    - IP-CIDR,0.0.0.0/32,DIRECT,no-resolve # 0.0.0.0 直连
    - IP-CIDR,10.0.0.0/8,DIRECT,no-resolve # 内网 A 类
    - IP-CIDR,172.16.0.0/12,DIRECT,no-resolve # 内网 B 类
    - IP-CIDR,192.168.0.0/16,DIRECT,no-resolve # 内网 C 类
    - IP-CIDR,127.0.0.0/8,DIRECT,no-resolve # 回环地址
    - IP-CIDR,::1/128,DIRECT,no-resolve # IPv6 回环
    - IP-CIDR,fc00::/7,DIRECT,no-resolve # IPv6 内网
  suffix:
proxies:
  prefix:
  suffix:
  override:
proxy-groups:
  prefix:
  suffix:
  override:
tun:
  enable: false
  stack: system
  auto-route: true
  auto-redir: true # clash
  auto-redirect: true # mihomo
  auto-detect-interface: true
  dns-hijack:
    - any:53
    - tcp://any:53
  strict-route: true
  route-exclude-address:
    - 1.1.1.1/32 # 用于 clashui 获取真实内网 IP
    - 127.0.0.1/32
  exclude-interface:
    - docker0 # 避免无法访问容器内服务
    - podman0
dns:
  enable: true
  listen: 0.0.0.0:1053
  enhanced-mode: fake-ip
  default-nameserver:
    - 223.5.5.5
    - 119.29.29.29
  proxy-server-nameserver:
    - 223.5.5.5
    - 119.29.29.29
    - 1.1.1.1
    - 8.8.8.8
  nameserver:
    - 114.114.114.114
    - 8.8.8.8
`

// DefaultEnv 是首次安装时写出的 .env 模板。
const DefaultEnv = `# 安装内核 可选：mihomo
KERNEL_NAME=mihomo

# 安装路径
CLASH_BASE_DIR=__BASEDIR__

# 机场订阅
CLASH_CONFIG_URL=

# 下载订阅时的 UserAgent
CLASH_SUB_UA=clash-verge/v2.4.0

# init 系统
INIT_TYPE=systemd

# Web UI 压缩包
ZIP_UI=resources/zip/dist.zip

# github 加速（留空则直连）
URL_GH_PROXY=

# 公共 Web 控制台
URL_CLASH_UI=http://board.zash.run.place

# 软件版本号
VERSION_MIHOMO=__VERSION_MIHOMO__
`

// UnitTemplate 是 systemd unit 内容。与 bash 版 scripts/init/systemd.sh 一致。
const UnitTemplate = `[Unit]
Description=__KERNEL__ Daemon, A[nother] Clash Kernel.
After=network.target NetworkManager.service systemd-networkd.service iwd.service

[Service]
Type=simple
LimitNPROC=500
LimitNOFILE=1000000
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE CAP_SYS_TIME CAP_SYS_PTRACE CAP_DAC_READ_SEARCH CAP_DAC_OVERRIDE
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE CAP_SYS_TIME CAP_SYS_PTRACE CAP_DAC_READ_SEARCH CAP_DAC_OVERRIDE
Restart=always
ExecStartPre=/usr/bin/sleep 1s
ExecStart=__KERNEL_PATH__ -d __RESOURCES__ -f __RUNTIME__
ExecReload=/bin/kill -HUP $MAINPID

[Install]
WantedBy=multi-user.target
`

// Run 执行完整安装。
func Run(ctx context.Context, o Options, rep Reporter) error {
	if o.BaseDir == "" {
		return fmt.Errorf("必须指定安装目录")
	}
	abs, err := filepath.Abs(o.BaseDir)
	if err != nil {
		return err
	}
	o.BaseDir = abs
	if o.MixedPort == 0 {
		o.MixedPort = 7890
	}
	if o.APIPort == 0 {
		o.APIPort = 9090
	}
	if o.UIURL == "" {
		o.UIURL = DefaultUIURL
	}
	o.UIURL = withProxy(o.UIURL, o.GhProxy)
	if o.Secret == "" {
		o.Secret = randomSecret()
	}

	// 1. 前置检查
	if err := preflight(o, rep); err != nil {
		return err
	}
	if o.DryRun {
		rep.Detail("安装目录：%s", o.BaseDir)
		rep.Detail("内核：%s", kernelURL(o))
		rep.Detail("面板：%s", o.UIURL)
		rep.Detail("端口：mixed %d，API %d", o.MixedPort, o.APIPort)
		rep.Detail("（--dry-run，未做任何改动）")
		return nil
	}

	// 2. 建目录骨架
	if err := mkDirs(o); err != nil {
		return err
	}
	rep.Step("创建目录骨架")

	// 3. 内核
	if err := installKernel(ctx, o, rep); err != nil {
		return err
	}

	// 4. 面板
	if err := installUI(ctx, o, rep); err != nil {
		// 面板失败不致命：内核能跑，面板可以之后 `clashgo ui update`
		rep.Detail("面板安装失败（不影响内核）：%v", err)
	} else {
		rep.Step("安装 zashboard 面板")
	}

	// 5. 配置文件
	if err := writeConfigs(o); err != nil {
		return err
	}
	rep.Step("生成 mixin.yaml 与 .env")

	// 6. systemd unit
	if err := installUnit(o, rep); err != nil {
		return err
	}

	return nil
}

func preflight(o Options, rep Reporter) error {
	// 安装目录已存在且非空 → 需要 --force
	if entries, err := os.ReadDir(o.BaseDir); err == nil && len(entries) > 0 && !o.Force {
		return fmt.Errorf("安装目录已存在且非空：%s（加 --force 覆盖）", o.BaseDir)
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("安装需要 root 权限（要写 /etc/systemd/system）")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return fmt.Errorf("当前系统没有 systemd，clashgo install 只支持 systemd")
	}
	for _, bin := range []string{"curl", "tar", "gzip"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("缺少命令 %s，请先安装", bin)
		}
	}
	rep.Step("前置检查通过")
	return nil
}

func mkDirs(o Options) error {
	for _, d := range []string{
		o.BaseDir,
		filepath.Join(o.BaseDir, "bin"),
		filepath.Join(o.BaseDir, "resources"),
		filepath.Join(o.BaseDir, "resources", "profiles"),
		filepath.Join(o.BaseDir, "resources", "zip"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// cpuLevel 返回 mihomo 的 CPU 优化档位：v1 / v2 / v3。
// 与 bash 版一致：看 /proc/cpuinfo 的 flags。
func cpuLevel() string {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "v1"
	}
	flags := ""
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "flags") {
			flags = line
			break
		}
	}
	has := func(f string) bool {
		for _, tok := range strings.Fields(flags) {
			if tok == f {
				return true
			}
		}
		return false
	}
	switch {
	case has("avx2") && has("fma"):
		return "v3"
	case has("sse4_2") && has("popcnt"):
		return "v2"
	default:
		return "v1"
	}
}

// kernelURL 推断内核下载地址。
func kernelURL(o Options) string {
	if o.KernelURL != "" {
		return withProxy(o.KernelURL, o.GhProxy)
	}
	ver := DefaultKernelVersion
	arch := ""
	switch runtime.GOARCH {
	case "amd64":
		arch = "amd64-" + cpuLevel() + "-" + ver
	case "arm64":
		arch = "arm64-" + ver
	case "386":
		arch = "386-" + ver
	default:
		arch = runtime.GOARCH + "-" + ver
	}
	return withProxy(fmt.Sprintf("https://github.com/MetaCubeX/mihomo/releases/download/%s/mihomo-linux-%s.gz", ver, arch), o.GhProxy)
}

// withProxy 给 GitHub 地址套上加速前缀。
// 约定与 bash 版一致：前缀 + "/" + 原始 URL。
func withProxy(url, prefix string) string {
	if prefix == "" || !strings.Contains(url, "github.com") {
		return url
	}
	return strings.TrimSuffix(prefix, "/") + "/" + url
}

func installKernel(ctx context.Context, o Options, rep Reporter) error {
	target := filepath.Join(o.BaseDir, "bin", "mihomo")
	if _, err := os.Stat(target); err == nil {
		rep.Skip("下载内核", "已存在 "+target)
		return nil
	}
	url := kernelURL(o)
	rep.Detail("内核地址：%s", url)
	raw, err := httpGet(ctx, url, rep, "内核")
	if err != nil {
		return fmt.Errorf("下载内核失败: %w", err)
	}
	gz, err := gzip.NewReader(strings.NewReader(string(raw)))
	if err != nil {
		return fmt.Errorf("内核不是 gzip 格式: %w", err)
	}
	defer gz.Close()
	bin, err := io.ReadAll(gz)
	if err != nil {
		return fmt.Errorf("解压内核失败: %w", err)
	}
	if err := os.WriteFile(target, bin, 0o755); err != nil {
		return err
	}
	// 校验：mihomo -v 能跑通才算装成功
	if out, err := exec.Command(target, "-v").CombinedOutput(); err != nil {
		os.Remove(target)
		return fmt.Errorf("内核自检失败: %v（输出：%s）", err, strings.TrimSpace(string(out)))
	}
	rep.Step("下载并安装内核")
	return nil
}

func installUI(ctx context.Context, o Options, rep Reporter) error {
	dist := filepath.Join(o.BaseDir, "resources", "dist")
	if _, err := os.Stat(filepath.Join(dist, "index.html")); err == nil {
		rep.Skip("下载面板", "已存在 "+dist)
		return nil
	}
	zipPath := filepath.Join(o.BaseDir, "resources", "zip", "dist.zip")
	rep.Detail("面板地址：%s", o.UIURL)
	data, err := httpGet(ctx, o.UIURL, rep, "面板")
	if err != nil {
		return err
	}
	if err := os.WriteFile(zipPath, data, 0o644); err != nil {
		return err
	}
	// 复用 ui 包的解压逻辑：剥掉顶层 dist/ 目录，防穿越
	return extractDist(data, dist)
}

func writeConfigs(o Options) error {
	envPath := filepath.Join(o.BaseDir, ".env")
	if _, err := os.Stat(envPath); err == nil && !o.Force {
		// 已存在就不覆盖（保护用户的 CLASH_BASE_DIR 等设置）
	} else {
		content := strings.ReplaceAll(DefaultEnv, "__BASEDIR__", o.BaseDir)
		content = strings.ReplaceAll(content, "__VERSION_MIHOMO__", DefaultKernelVersion)
		if err := os.WriteFile(envPath, []byte(content), 0o644); err != nil {
			return err
		}
	}

	mixinPath := filepath.Join(o.BaseDir, "resources", "mixin.yaml")
	if _, err := os.Stat(mixinPath); err != nil || o.Force {
		content := strings.ReplaceAll(DefaultMixin, "__SECRET__", o.Secret)
		content = strings.ReplaceAll(content, "7890", fmt.Sprint(o.MixedPort))
		content = strings.ReplaceAll(content, "0.0.0.0:9090", fmt.Sprintf("0.0.0.0:%d", o.APIPort))
		if err := os.WriteFile(mixinPath, []byte(content), 0o644); err != nil {
			return err
		}
	}

	// config.yaml 与 profiles.yaml 留空/空清单，等用户 clashgo sub add
	cfgPath := filepath.Join(o.BaseDir, "resources", "config.yaml")
	if _, err := os.Stat(cfgPath); err != nil || o.Force {
		if err := os.WriteFile(cfgPath, []byte{}, 0o644); err != nil {
			return err
		}
	}
	metaPath := filepath.Join(o.BaseDir, "resources", "profiles.yaml")
	if _, err := os.Stat(metaPath); err != nil || o.Force {
		if err := os.WriteFile(metaPath, []byte("profiles: []\n"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func installUnit(o Options, rep Reporter) error {
	name := "mihomo"
	unitDir := resolveUnitDir(o)
	unitPath := filepath.Join(unitDir, name+".service")
	if _, err := os.Stat(unitPath); err == nil && !o.Force {
		rep.Skip("安装 systemd unit", "已存在 "+unitPath)
		return nil
	}
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return err
	}
	content := UnitTemplate
	content = strings.ReplaceAll(content, "__KERNEL__", name)
	content = strings.ReplaceAll(content, "__KERNEL_PATH__", filepath.Join(o.BaseDir, "bin", name))
	content = strings.ReplaceAll(content, "__RESOURCES__", filepath.Join(o.BaseDir, "resources"))
	content = strings.ReplaceAll(content, "__RUNTIME__", filepath.Join(o.BaseDir, "resources", "runtime.yaml"))
	if err := os.WriteFile(unitPath, []byte(content), 0o644); err != nil {
		return err
	}
	// 只有落到标准目录时才让 systemd 感知；自定义目录（测试/非标准部署）只写文件。
	if unitDir != DefaultUnitDir {
		rep.Step("写入 systemd unit（" + unitPath + "）")
		return nil
	}
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload 失败: %v（%s）", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("systemctl", "enable", name).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl enable %s 失败: %v（%s）", name, err, strings.TrimSpace(string(out)))
	}
	rep.Step("安装并启用 systemd unit")
	return nil
}

// Progresser 是可选能力：Reporter 实现了它就汇报下载进度。
// 大文件下载（内核 30MB、面板 9MB）不打印进度时看起来就是卡死，
// 用户会以为命令挂了。
type Progresser interface {
	Progress(label string, done, total int64)
}

// progress 把进度转给 Reporter（如果它支持）。
func progress(rep Reporter, label string, done, total int64) {
	if p, ok := rep.(Progresser); ok {
		p.Progress(label, done, total)
	}
}

// httpGet 流式下载到内存，带进度、停滞检测与总时长上限。
//
// 代理策略用 ProxyFromEnvironment：装了 clashctl/clashgo 的机器上
// rc 文件里本来就有 http_proxy，此时直连 GitHub 反而更慢或直接失败
// （实测本机直连 release 返回 000）。没有代理变量时自然就是直连。
//
// stallTimeout 是「多久没收到数据就判失败」：GitHub 的 CDN 在国内
// 经常建连成功但后续不收数据，只靠整体超时会让命令看起来卡死。
func httpGet(ctx context.Context, url string, rep Reporter, label string) ([]byte, error) {
	if strings.HasPrefix(url, "file://") {
		return os.ReadFile(strings.TrimPrefix(url, "file://"))
	}

	// stallCtx 同时作为请求的 ctx：请求挂在它上面，定时器一取消，
	// 阻塞中的 Body.Read 才会立刻返回。只取消一个旁路 ctx 是没用的。
	stallCtx, cancelStall := context.WithCancel(ctx)
	defer cancelStall()
	timer := time.AfterFunc(stallTimeout(), cancelStall)
	defer timer.Stop()

	client := &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
		Timeout: 20 * time.Minute,
	}
	req, err := http.NewRequestWithContext(stallCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "clashgo-install")
	resp, err := client.Do(req)
	if err != nil {
		return nil, stallErr(ctx, err, 0)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	total := resp.ContentLength
	var buf bytes.Buffer
	if total > 0 {
		buf.Grow(int(min64(total, 200<<20)))
	}
	chunk := make([]byte, 64<<10)
	// 节流：最多每 200ms 汇报一次，否则 9MB 文件会刷出上百行进度
	var lastReport time.Time
	for {
		n, rerr := resp.Body.Read(chunk)
		if n > 0 {
			timer.Reset(stallTimeout())
			buf.Write(chunk[:n])
			if time.Since(lastReport) >= 200*time.Millisecond {
				progress(rep, label, int64(buf.Len()), total)
				lastReport = time.Now()
			}
			if buf.Len() > 200<<20 {
				return nil, fmt.Errorf("下载内容超过 200MB，已中止")
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return nil, stallErr(ctx, rerr, int64(buf.Len()))
		}
	}
	progress(rep, label, int64(buf.Len()), total)
	return buf.Bytes(), nil
}

// stallTimeout 是「多久没收到数据就判失败」。
// 做成变量是为了测试能把它调小，生产值 30s。
var stallTimeout = func() time.Duration { return 30 * time.Second }

// stallErr 判断这次失败是不是「卡住」造成的，并给出能指导动作的错误文案。
func stallErr(ctx context.Context, err error, got int64) error {
	if ctx.Err() == nil && (errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "context canceled")) {
		return fmt.Errorf("下载停滞：%s 没有收到数据（已收到 %d 字节）", stallTimeout(), got)
	}
	return err
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// extractDist 解压 dist.zip，剥掉共同的顶层目录（dist/）。
// 复用 ui 包的实现，保持「安装」与「更新」两条路径行为一致。
func extractDist(data []byte, dest string) error {
	if _, err := ui.ExtractZip(data, dest); err != nil {
		return err
	}
	return nil
}

// randomSecret 生成 6 位随机密钥。
func randomSecret() string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "clashgo"
	}
	out := make([]byte, 6)
	for i, b := range buf {
		out[i] = chars[int(b)%len(chars)]
	}
	return string(out)
}
