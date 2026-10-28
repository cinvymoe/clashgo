package install

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rep 记录汇报，测试里断言「哪些步骤跑了」。
type rep struct {
	steps   []string
	skips   []string
	details []string
}

func (r *rep) Step(name string) { r.steps = append(r.steps, name) }
func (r *rep) Skip(name, reason string) {
	r.skips = append(r.skips, name+": "+reason)
}
func (r *rep) Detail(format string, args ...any) {
	r.details = append(r.details, format)
}

func (r *rep) hasStep(sub string) bool {
	for _, s := range r.steps {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestCPULevelIsValid(t *testing.T) {
	got := cpuLevel()
	switch got {
	case "v1", "v2", "v3":
	default:
		t.Fatalf("cpuLevel() = %q，只允许 v1/v2/v3", got)
	}
}

func TestKernelURLInfersArch(t *testing.T) {
	url := kernelURL(Options{})
	for _, want := range []string{"github.com/MetaCubeX/mihomo/releases/download/", "mihomo-linux-", ".gz"} {
		if !strings.Contains(url, want) {
			t.Errorf("kernelURL() = %q，缺少 %q", url, want)
		}
	}
	// 显式指定时必须原样返回
	custom := "https://example.com/mihomo.gz"
	if got := kernelURL(Options{KernelURL: custom}); got != custom {
		t.Errorf("kernelURL(显式) = %q，期望 %q", got, custom)
	}
}

func TestRandomSecret(t *testing.T) {
	a, b := randomSecret(), randomSecret()
	if len(a) != 6 {
		t.Errorf("密钥长度 = %d，期望 6", len(a))
	}
	if a == b {
		t.Errorf("两次随机密钥相同：%q", a)
	}
	for _, c := range a {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", c) {
			t.Errorf("密钥含非法字符 %q", c)
		}
	}
}

func TestMkDirsCreatesSkeleton(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "inst")
	if err := mkDirs(Options{BaseDir: base}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"bin", "resources", "resources/profiles", "resources/zip"} {
		if _, err := os.Stat(filepath.Join(base, d)); err != nil {
			t.Errorf("缺少目录 %s: %v", d, err)
		}
	}
}

func TestWriteConfigs(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "inst")
	if err := mkDirs(Options{BaseDir: base}); err != nil {
		t.Fatal(err)
	}
	o := Options{BaseDir: base, Secret: "abc123", MixedPort: 7899, APIPort: 9191}
	if err := writeConfigs(o); err != nil {
		t.Fatal(err)
	}

	env, err := os.ReadFile(filepath.Join(base, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CLASH_BASE_DIR=" + base, "KERNEL_NAME=mihomo", "INIT_TYPE=systemd", "VERSION_MIHOMO=" + DefaultKernelVersion} {
		if !strings.Contains(string(env), want) {
			t.Errorf(".env 缺少 %q\n%s", want, env)
		}
	}

	mixin, err := os.ReadFile(filepath.Join(base, "resources", "mixin.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"secret: abc123", "mixed-port: 7899", "0.0.0.0:9191", "external-ui: dist"} {
		if !strings.Contains(string(mixin), want) {
			t.Errorf("mixin.yaml 缺少 %q", want)
		}
	}
	// 注释必须保留（用户靠这些注释理解 prefix/suffix/override/inject）
	for _, want := range []string{"# prefix：前置插入", "#authentication:", "#  - \"admin:admin123\""} {
		if !strings.Contains(string(mixin), want) {
			t.Errorf("mixin.yaml 丢失注释 %q", want)
		}
	}

	if _, err := os.Stat(filepath.Join(base, "resources", "profiles.yaml")); err != nil {
		t.Errorf("缺少 profiles.yaml: %v", err)
	}
}

func TestWriteConfigsKeepsExisting(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "inst")
	if err := mkDirs(Options{BaseDir: base}); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(base, ".env")
	if err := os.WriteFile(envPath, []byte("CLASH_BASE_DIR=/custom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 不带 Force：已有 .env 不能被覆盖
	if err := writeConfigs(Options{BaseDir: base, Secret: "x"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(envPath)
	if string(got) != "CLASH_BASE_DIR=/custom\n" {
		t.Errorf("已存在的 .env 被覆盖了：%q", got)
	}
}

func TestUnitTemplateRenders(t *testing.T) {
	content := UnitTemplate
	for _, kv := range [][2]string{
		{"__KERNEL__", "mihomo"},
		{"__KERNEL_PATH__", "/x/bin/mihomo"},
		{"__RESOURCES__", "/x/resources"},
		{"__RUNTIME__", "/x/resources/runtime.yaml"},
	} {
		content = strings.ReplaceAll(content, kv[0], kv[1])
	}
	for _, want := range []string{
		"ExecStart=/x/bin/mihomo -d /x/resources -f /x/resources/runtime.yaml",
		"ExecReload=/bin/kill -HUP $MAINPID",
		"Restart=always",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("unit 缺少 %q", want)
		}
	}
	if strings.Contains(content, "__") {
		t.Errorf("unit 还有未替换的占位符：%s", content)
	}
}

func TestPreflightRejectsNonEmptyDir(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "inst")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &rep{}
	err := preflight(Options{BaseDir: base}, r)
	if err == nil {
		t.Fatal("非空目录应当被拒绝")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("错误信息应提示 --force，实际：%v", err)
	}
	// 加 Force 后应当通过（后续检查在测试环境里可能不满足，只要不是这条错即可）
	err = preflight(Options{BaseDir: base, Force: true}, r)
	if err != nil && strings.Contains(err.Error(), "已存在且非空") {
		t.Errorf("--force 未生效：%v", err)
	}
}

func TestExtractDistStripsTopDir(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"dist/index.html", "dist/assets/app.js"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "dist")
	if err := extractDist(buf.Bytes(), dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "index.html")); err != nil {
		t.Errorf("剥掉顶层目录后应有 index.html: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "dist")); err == nil {
		t.Error("不应出现 dist/dist 嵌套")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "inst")
	r := &rep{}
	err := Run(context.Background(), Options{BaseDir: base, DryRun: true}, r)
	if err != nil {
		// 非 root 或没有 systemd 时前置检查会失败，这是预期行为
		if strings.Contains(err.Error(), "root") || strings.Contains(err.Error(), "systemd") {
			t.Skipf("环境不满足前置检查：%v", err)
		}
		t.Fatal(err)
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Errorf("--dry-run 不应创建安装目录，实际存在 %s", base)
	}
	if len(r.details) == 0 {
		t.Error("--dry-run 应打印计划")
	}
}

func TestRunFullInstallInSandbox(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("需要 root（preflight 会检查）")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		t.Skip("环境没有 systemd")
	}
	root := t.TempDir()
	base := filepath.Join(root, "inst")
	unitDir := filepath.Join(root, "units")

	// 用本地文件伪装内核与面板包，避免测试依赖网络
	kernelGz := filepath.Join(root, "mihomo.gz")
	if err := os.WriteFile(kernelGz, gzipOf(t, "/bin/sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(root, "dist.zip")
	if err := os.WriteFile(zipPath, zipOf(t), 0o644); err != nil {
		t.Fatal(err)
	}

	r := &rep{}
	err := Run(context.Background(), Options{
		BaseDir:   base,
		KernelURL: "file://" + kernelGz,
		UIURL:     "file://" + zipPath,
		Secret:    "s3cret",
		UnitDir:   unitDir,
	}, r)
	if err != nil {
		t.Fatalf("安装失败: %v", err)
	}

	// 目录与文件
	for _, p := range []string{
		".env", "bin/mihomo", "resources/mixin.yaml", "resources/config.yaml",
		"resources/profiles.yaml", "resources/dist/index.html",
		"resources/zip/dist.zip", filepath.Join(unitDir, "mihomo.service"),
	} {
		if _, err := os.Stat(filepath.Join(base, p)); err != nil {
			if p == filepath.Join(unitDir, "mihomo.service") {
				if _, err2 := os.Stat(filepath.Join(unitDir, "mihomo.service")); err2 == nil {
					continue
				}
			}
			t.Errorf("安装后缺少 %s: %v", p, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(base, "bin", "mihomo")); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("内核没有可执行位：%v", fi.Mode())
	}
	for _, want := range []string{"创建目录骨架", "下载并安装内核", "安装 zashboard 面板", "生成 mixin.yaml 与 .env"} {
		if !r.hasStep(want) {
			t.Errorf("缺少步骤汇报 %q，实际 %v", want, r.steps)
		}
	}
}

func TestRunInstallIdempotentSkips(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("需要 root")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		t.Skip("环境没有 systemd")
	}
	root := t.TempDir()
	base := filepath.Join(root, "inst")
	unitDir := filepath.Join(root, "units")
	kernelGz := filepath.Join(root, "mihomo.gz")
	if err := os.WriteFile(kernelGz, gzipOf(t, "/bin/sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(root, "dist.zip")
	if err := os.WriteFile(zipPath, zipOf(t), 0o644); err != nil {
		t.Fatal(err)
	}
	o := Options{BaseDir: base, KernelURL: "file://" + kernelGz, UIURL: "file://" + zipPath, Secret: "s", UnitDir: unitDir}
	if err := Run(context.Background(), o, &rep{}); err != nil {
		t.Fatal(err)
	}
	// 第二次：内核与面板已存在，应当走 Skip
	o.Force = true // 允许覆盖已存在目录
	r2 := &rep{}
	if err := Run(context.Background(), o, r2); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(r2.skips, "|"), "下载内核") {
		t.Errorf("第二次安装应跳过内核下载，skips=%v", r2.skips)
	}
	if !strings.Contains(strings.Join(r2.skips, "|"), "下载面板") {
		t.Errorf("第二次安装应跳过面板下载，skips=%v", r2.skips)
	}
}

// gzipOf 把文件内容压成 gzip 字节。
func gzipOf(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// zipOf 造一个含 dist/ 顶层目录的最小面板包。
func zipOf(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"dist/index.html", "dist/assets/app.js"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestWithProxy(t *testing.T) {
	gh := "https://ghproxy.com"
	if got := withProxy("https://github.com/a/b.gz", gh); got != gh+"/https://github.com/a/b.gz" {
		t.Errorf("withProxy = %q", got)
	}
	// 前缀带尾斜杠要去掉，避免 //github.com
	if got := withProxy("https://github.com/a/b.gz", gh+"/"); got != gh+"/https://github.com/a/b.gz" {
		t.Errorf("withProxy 尾斜杠未处理：%q", got)
	}
	// 空前缀与非 GitHub 地址原样返回
	if got := withProxy("https://github.com/a/b.gz", ""); got != "https://github.com/a/b.gz" {
		t.Errorf("空前缀不应改写：%q", got)
	}
	if got := withProxy("https://example.com/x", gh); got != "https://example.com/x" {
		t.Errorf("非 GitHub 地址不应改写：%q", got)
	}
}

func TestProgressIsThrottled(t *testing.T) {
	// 9MB 文件按 64KB 分块会调用 144 次；节流后应显著少于这个数
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4194304")
		chunk := bytes.Repeat([]byte("x"), 64<<10)
		for i := 0; i < 64; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	r := &countingRep{}
	if _, err := httpGet(context.Background(), srv.URL, r, "测试"); err != nil {
		t.Fatal(err)
	}
	if r.calls == 0 {
		t.Fatal("应当至少汇报一次进度")
	}
	if r.calls > 40 {
		t.Errorf("进度汇报 %d 次，节流失效（应远小于 64）", r.calls)
	}
	// 最后一次必须是完整的 4MB
	if r.last != 4194304 {
		t.Errorf("最后一次进度 = %d，期望 4194304", r.last)
	}
}

func TestHTTPGetStallDetected(t *testing.T) {
	// 服务端发一点数据后卡住不发 EOF，也不关闭连接
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		_, _ = w.Write(bytes.Repeat([]byte("x"), 1024))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer func() { close(release); srv.Close() }()

	// 把停滞阈值调到 200ms，测试不必真等 30 秒
	old := stallTimeout
	stallTimeout = func() time.Duration { return 200 * time.Millisecond }
	defer func() { stallTimeout = old }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	_, err := httpGet(ctx, srv.URL, &rep{}, "停滞")
	if err == nil {
		t.Fatal("停滞的下载应当报错")
	}
	if !strings.Contains(err.Error(), "停滞") {
		t.Errorf("错误应说明停滞，实际：%v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("停滞检测太慢：%v", d)
	}
}

type countingRep struct {
	calls int
	last  int64
}

func (c *countingRep) Step(string)           {}
func (c *countingRep) Skip(string, string)   {}
func (c *countingRep) Detail(string, ...any) {}
func (c *countingRep) Progress(_ string, done, _ int64) {
	c.calls++
	c.last = done
}
