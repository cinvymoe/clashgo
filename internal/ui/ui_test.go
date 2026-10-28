package ui

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"clashgo/internal/env"
)

// makeZip 造一个 zip：entries 是 name→内容。
func makeZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func testEnv(t *testing.T) *env.Env {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "resources", "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(root, ".env")
	if err := os.WriteFile(envFile, []byte("KERNEL_NAME=mihomo\nCLASH_BASE_DIR="+root+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLASHGO_ENV", envFile)
	e, err := env.Load()
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// TestInstallStripsTopDir zashboard 的 release 包顶层是 dist/，安装时必须剥掉。
func TestInstallStripsTopDir(t *testing.T) {
	e := testEnv(t)
	m := New(e)
	data := makeZip(t, map[string]string{
		"dist/index.html":     "<html>zashboard</html>",
		"dist/assets/app.js":  "console.log(1)",
		"dist/favicon.ico":    "icon",
		"dist/assets/app.css": "body{}",
	})
	n, err := m.InstallFromBytes(data)
	if err != nil {
		t.Fatalf("InstallFromBytes: %v", err)
	}
	if n != 4 {
		t.Errorf("应安装 4 个文件，实际 %d", n)
	}
	// 关键：index.html 必须在 dist 根下，而不是 dist/dist/
	idx := filepath.Join(e.DistDir(), "index.html")
	if _, err := os.Stat(idx); err != nil {
		t.Errorf("index.html 应在 %s: %v", idx, err)
	}
	if _, err := os.Stat(filepath.Join(e.DistDir(), "assets", "app.js")); err != nil {
		t.Errorf("assets/app.js 应存在: %v", err)
	}
}

// TestInstallKeepsFlatZip 没有共同顶层目录的 zip 应原样展开。
func TestInstallKeepsFlatZip(t *testing.T) {
	e := testEnv(t)
	m := New(e)
	data := makeZip(t, map[string]string{
		"index.html":   "<html/>",
		"assets/a.js":  "x",
		"manifest.web": "m",
	})
	if _, err := m.InstallFromBytes(data); err != nil {
		t.Fatalf("InstallFromBytes: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.DistDir(), "index.html")); err != nil {
		t.Errorf("index.html 应直接落在 dist 下: %v", err)
	}
}

// TestInstallRejectsZipWithoutIndex 没有 index.html 的包不是前端包，必须拒绝。
func TestInstallRejectsZipWithoutIndex(t *testing.T) {
	e := testEnv(t)
	m := New(e)
	data := makeZip(t, map[string]string{"dist/readme.txt": "hi"})
	if _, err := m.InstallFromBytes(data); err == nil {
		t.Fatal("缺少 index.html 应报错")
	}
	// 失败时不应留下 dist.new
	if _, err := os.Stat(e.DistDir() + ".new"); !os.IsNotExist(err) {
		t.Error("失败后不应残留 dist.new")
	}
}

// TestInstallReplacesOld 替换已有面板后旧文件不应残留。
func TestInstallReplacesOld(t *testing.T) {
	e := testEnv(t)
	old := filepath.Join(e.DistDir(), "old-only.js")
	if err := os.WriteFile(filepath.Join(e.DistDir(), "index.html"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := New(e)
	data := makeZip(t, map[string]string{
		"dist/index.html": "<html>new</html>",
	})
	if _, err := m.InstallFromBytes(data); err != nil {
		t.Fatalf("InstallFromBytes: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(e.DistDir(), "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "<html>new</html>" {
		t.Errorf("index.html 应为新内容，实际 %q", content)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("旧面板的残留文件应被清掉")
	}
}

// TestInstalled 检测 dist 是否可用。
func TestInstalled(t *testing.T) {
	e := testEnv(t)
	m := New(e)
	if m.Installed() {
		t.Error("空 dist 应报告未安装")
	}
	if err := os.WriteFile(filepath.Join(e.DistDir(), "index.html"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !m.Installed() {
		t.Error("有 index.html 应报告已安装")
	}
}

// TestInstallOverHTTP 走真实下载路径。
func TestInstallOverHTTP(t *testing.T) {
	e := testEnv(t)
	data := makeZip(t, map[string]string{"dist/index.html": "<html/>"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	m := New(e)
	m.URL = srv.URL
	if _, err := m.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !m.Installed() {
		t.Error("安装后应检测到面板")
	}
}

// TestInstallHTTPError 下载失败时不能破坏已有面板。
func TestInstallHTTPError(t *testing.T) {
	e := testEnv(t)
	keep := filepath.Join(e.DistDir(), "index.html")
	if err := os.WriteFile(keep, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	m := New(e)
	m.URL = srv.URL
	if _, err := m.Install(); err == nil {
		t.Fatal("HTTP 404 应报错")
	}
	content, err := os.ReadFile(keep)
	if err != nil {
		t.Fatalf("旧面板应保留: %v", err)
	}
	if string(content) != "keep" {
		t.Errorf("旧面板内容被改动: %q", content)
	}
}

// TestRejectZipSlip zip 内的穿越路径必须被拒绝。
func TestRejectZipSlip(t *testing.T) {
	e := testEnv(t)
	m := New(e)
	// 直接构造一个 ../ 条目
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("dist/../../evil.txt")
	_, _ = w.Write([]byte("pwned"))
	_ = zw.Close()

	if _, err := m.InstallFromBytes(buf.Bytes()); err == nil {
		t.Fatal("穿越路径应被拒绝")
	}
	escape := filepath.Join(e.BaseDir, "..", "evil.txt")
	if _, err := os.Stat(escape); err == nil {
		_ = os.Remove(escape)
		t.Error("不应把文件写到 dist 之外")
	}
}
