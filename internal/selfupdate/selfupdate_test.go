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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture 是一套假的 GitHub Releases 服务。
type fixture struct {
	srv   *httptest.Server
	tag   string
	bin   []byte  // tar.gz 里的 clashgo 内容
	asset []byte  // 上传的产物字节
	sums  *string // checksums.txt 内容；nil 表示不提供
	pre   bool    // 是否标成预发布
	hits  map[string]int
}

func newFixture(t *testing.T, bin []byte) *fixture {
	t.Helper()
	f := &fixture{tag: "v0.2.0", bin: bin, hits: map[string]int{}}
	f.asset = tarGz(t, "clashgo", bin)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases/latest", f.handleRelease)
	mux.HandleFunc("/repos/o/r/releases/tags/", f.handleRelease)
	mux.HandleFunc("/dl/asset.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		f.hits["asset"]++
		w.Write(f.asset)
	})
	mux.HandleFunc("/dl/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		f.hits["checksums"]++
		if f.sums == nil {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, *f.sums)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) handleRelease(w http.ResponseWriter, r *http.Request) {
	f.hits["release"]++
	if strings.Contains(r.URL.Path, "/tags/") && !strings.HasSuffix(r.URL.Path, f.tag) {
		http.NotFound(w, r)
		return
	}
	assets := []Asset{{
		Name: AssetName(),
		Size: int64(len(f.asset)),
		URL:  f.srv.URL + "/dl/asset.tar.gz",
	}}
	if f.sums != nil {
		assets = append(assets, Asset{
			Name: ChecksumAsset,
			Size: int64(len(*f.sums)),
			URL:  f.srv.URL + "/dl/checksums.txt",
		})
	}
	rel := Release{TagName: f.tag, Name: f.tag, Prerelease: f.pre, Assets: assets}
	_ = json.NewEncoder(w).Encode(rel)
}

func (f *fixture) opts() Options {
	return Options{BaseURL: f.srv.URL, Repo: "o/r"}
}

// tarGz 造一个含单个可执行文件的 tar.gz。
func tarGz(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeSelf 在临时目录里造一个可执行的「当前版本」脚本，替代真实二进制。
func fakeSelf(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "clashgo")
	body := "#!/bin/sh\necho " + version + "\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAssetName(t *testing.T) {
	got := AssetName()
	if !strings.HasPrefix(got, "clashgo-") || !strings.HasSuffix(got, ".tar.gz") {
		t.Errorf("AssetName() = %q，格式应为 clashgo-<os>-<arch>.tar.gz", got)
	}
}

func TestFetchReleaseLatest(t *testing.T) {
	f := newFixture(t, []byte("x"))
	rel, err := FetchRelease(context.Background(), f.opts())
	if err != nil {
		t.Fatal(err)
	}
	if rel.TagName != "v0.2.0" {
		t.Errorf("TagName = %q", rel.TagName)
	}
	if len(rel.Assets) != 1 {
		t.Errorf("Assets 数量 = %d", len(rel.Assets))
	}
}

func TestFetchReleaseByTag(t *testing.T) {
	f := newFixture(t, []byte("x"))
	o := f.opts()
	o.Tag = "v0.2.0"
	if _, err := FetchRelease(context.Background(), o); err != nil {
		t.Fatalf("按 tag 查询失败: %v", err)
	}
	o.Tag = "v9.9.9"
	if _, err := FetchRelease(context.Background(), o); err == nil {
		t.Fatal("不存在的 tag 应当报错")
	} else if !strings.Contains(err.Error(), "o/r") {
		t.Errorf("错误里应带仓库名，实际：%v", err)
	}
}

func TestFetchReleaseNoReleases(t *testing.T) {
	// 仓库不存在 / 没有发布时 GitHub 返回 404
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	_, err := FetchRelease(context.Background(), Options{BaseURL: srv.URL, Repo: "o/none"})
	if err == nil {
		t.Fatal("404 应当报错")
	}
	if !strings.Contains(err.Error(), "还没有任何发布") {
		t.Errorf("错误文案不对：%v", err)
	}
}

func TestFetchReleasePrereleaseGated(t *testing.T) {
	f := newFixture(t, []byte("x"))
	f.pre = true
	if _, err := FetchRelease(context.Background(), f.opts()); err == nil {
		t.Fatal("预发布应当被拦下")
	} else if !strings.Contains(err.Error(), "--pre") {
		t.Errorf("错误应提示 --pre：%v", err)
	}
	o := f.opts()
	o.Pre = true
	if _, err := FetchRelease(context.Background(), o); err != nil {
		t.Fatalf("--pre 后应放行: %v", err)
	}
}

func TestPickAssetFallsBackToPlainBinary(t *testing.T) {
	rel := &Release{TagName: "v1", Assets: []Asset{
		{Name: "something-else.zip"},
		{Name: strings.TrimSuffix(AssetName(), ".tar.gz")},
	}}
	a, err := rel.PickAsset()
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(a.Name, ".tar.gz") {
		t.Errorf("应选裸二进制，实际 %q", a.Name)
	}
}

func TestPickAssetReportsAvailable(t *testing.T) {
	rel := &Release{TagName: "v1", Assets: []Asset{{Name: "a.zip"}, {Name: "b.zip"}}}
	_, err := rel.PickAsset()
	if err == nil {
		t.Fatal("缺产物应当报错")
	}
	for _, want := range []string{"a.zip", "b.zip", AssetName()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误里缺少 %q：%v", want, err)
		}
	}
}

func TestExtractBinaryFromTarGz(t *testing.T) {
	payload := []byte("#!/bin/sh\necho hi\n")
	got, err := ExtractBinary(tarGz(t, "clashgo", payload))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("解出的内容不一致：%q", got)
	}
}

func TestExtractBinaryFromTarGzNested(t *testing.T) {
	// 产物里带一层目录也要能找到
	got, err := ExtractBinary(tarGz(t, "dist/clashgo", []byte("bin")))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "bin" {
		t.Errorf("内容 = %q", got)
	}
}

func TestExtractBinaryPlainPassthrough(t *testing.T) {
	raw := []byte("not a gzip")
	got, err := ExtractBinary(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Error("裸二进制应原样返回")
	}
}

func TestExtractBinaryMissingClashgo(t *testing.T) {
	_, err := ExtractBinary(tarGz(t, "other", []byte("x")))
	if err == nil {
		t.Fatal("压缩包里没有 clashgo 应当报错")
	}
}

func TestUpdateReplacesBinary(t *testing.T) {
	newBin := []byte("#!/bin/sh\necho v0.2.0\n")
	f := newFixture(t, newBin)
	self := fakeSelf(t, "v0.1.0")

	res, err := Update(context.Background(), self, f.opts())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied {
		t.Fatal("应当完成替换")
	}
	if res.From != "v0.1.0" || res.To != "v0.2.0" {
		t.Errorf("From/To = %q/%q", res.From, res.To)
	}
	got, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, newBin) {
		t.Errorf("可执行文件内容未替换：%q", got)
	}
	// 备份要留着，出问题能手工回滚
	if _, err := os.Stat(self + ".bak"); err != nil {
		t.Errorf("缺少备份文件: %v", err)
	}
	// 替换后的文件必须仍然可执行
	fi, _ := os.Stat(self)
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("替换后丢了可执行位：%v", fi.Mode())
	}
}

func TestUpdateUpToDate(t *testing.T) {
	f := newFixture(t, []byte("#!/bin/sh\necho v0.2.0\n"))
	self := fakeSelf(t, "v0.2.0")
	res, err := Update(context.Background(), self, f.opts())
	if err != nil {
		t.Fatal(err)
	}
	if !res.UpToDate {
		t.Error("同版本应当报 UpToDate")
	}
	if f.hits["asset"] != 0 {
		t.Error("同版本不应下载产物")
	}
}

func TestUpdateDryRunDoesNotTouchFile(t *testing.T) {
	f := newFixture(t, []byte("#!/bin/sh\necho new\n"))
	self := fakeSelf(t, "v0.1.0")
	o := f.opts()
	o.DryRun = true
	res, err := Update(context.Background(), self, o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied {
		t.Error("--dry-run 不应替换")
	}
	got, _ := os.ReadFile(self)
	if !strings.Contains(string(got), "v0.1.0") {
		t.Errorf("文件被改动了：%q", got)
	}
}

func TestUpdateVerifiesChecksum(t *testing.T) {
	f := newFixture(t, []byte("#!/bin/sh\necho v0.2.0\n"))
	// 发布里的 checksums.txt 记的是 tar.gz 的哈希，不是解压后二进制的哈希
	sum := sha256.Sum256(f.asset)
	sums := hex.EncodeToString(sum[:]) + "  " + AssetName() + "\n"
	f.sums = &sums
	self := fakeSelf(t, "v0.1.0")

	res, err := Update(context.Background(), self, f.opts())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Verified {
		t.Error("有 checksums.txt 时应标记 Verified")
	}
	if f.hits["checksums"] == 0 {
		t.Error("应当下载 checksums.txt")
	}
}

// 有些项目发布的是裸二进制，checksums.txt 记的是二进制本身的哈希，也要能核对通过。
func TestUpdateVerifiesBinaryHashConvention(t *testing.T) {
	bin := []byte("#!/bin/sh\necho v0.2.0\n")
	f := newFixture(t, bin)
	sum := sha256.Sum256(bin)
	sums := hex.EncodeToString(sum[:]) + "  *" + AssetName() + "\n"
	f.sums = &sums
	res, err := Update(context.Background(), fakeSelf(t, "v0.1.0"), f.opts())
	if err != nil {
		t.Fatalf("应兼容二进制哈希：%v", err)
	}
	if !res.Verified || !res.Applied {
		t.Errorf("Verified=%v Applied=%v", res.Verified, res.Applied)
	}
}

func TestUpdateRejectsBadChecksum(t *testing.T) {
	bin := []byte("#!/bin/sh\necho v0.2.0\n")
	f := newFixture(t, bin)
	sums := strings.Repeat("0", 64) + "  " + AssetName() + "\n"
	f.sums = &sums
	self := fakeSelf(t, "v0.1.0")
	before, _ := os.ReadFile(self)

	_, err := Update(context.Background(), self, f.opts())
	if err == nil {
		t.Fatal("校验和不匹配必须报错")
	}
	if !strings.Contains(err.Error(), "校验和不匹配") {
		t.Errorf("错误文案不对：%v", err)
	}
	after, _ := os.ReadFile(self)
	if !bytes.Equal(before, after) {
		t.Error("校验失败时不应改动原文件")
	}
}

func TestUpdateRejectsMissingChecksumEntry(t *testing.T) {
	bin := []byte("#!/bin/sh\necho v0.2.0\n")
	f := newFixture(t, bin)
	sums := strings.Repeat("a", 64) + "  some-other-asset.tar.gz\n"
	f.sums = &sums
	_, err := Update(context.Background(), fakeSelf(t, "v0.1.0"), f.opts())
	if err == nil || !strings.Contains(err.Error(), "没有") {
		t.Fatalf("checksums.txt 缺条目应报错，实际：%v", err)
	}
}

func TestReplaceRejectsBrokenBinary(t *testing.T) {
	self := fakeSelf(t, "v0.1.0")
	before, _ := os.ReadFile(self)
	// 自检跑不起来 → 必须回滚，原文件不能动
	err := Replace(self, []byte("not an executable"), false)
	if err == nil {
		t.Fatal("坏包应当被自检拦下")
	}
	if !strings.Contains(err.Error(), "自检失败") {
		t.Errorf("错误文案不对：%v", err)
	}
	after, _ := os.ReadFile(self)
	if !bytes.Equal(before, after) {
		t.Error("自检失败后原文件被改动了")
	}
	if _, err := os.Stat(self + ".bak"); err == nil {
		t.Error("自检失败不应留下备份（说明还没进入替换阶段）")
	}
}

func TestReplaceNoVerifySkipsCheck(t *testing.T) {
	self := fakeSelf(t, "v0.1.0")
	body := []byte("#!/bin/sh\necho whatever\n")
	if err := Replace(self, body, true); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(self)
	if !bytes.Equal(got, body) {
		t.Error("NoVerify 应当跳过自检并完成替换")
	}
}

func TestNormalizeVersion(t *testing.T) {
	cases := map[string]string{
		"v0.2.0":        "0.2.0",
		"0.2.0":         "0.2.0",
		" v0.2.0 ":      "0.2.0",
		"0.2.0-3-gabc1": "0.2.0",
		"dev":           "dev",
	}
	for in, want := range cases {
		if got := NormalizeVersion(in); got != want {
			t.Errorf("NormalizeVersion(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestRepoOverridePrecedence(t *testing.T) {
	old := Repo
	Repo = "default/repo"
	defer func() { Repo = old }()

	t.Setenv(RepoEnv, "")
	if got := (Options{}).repo(); got != "default/repo" {
		t.Errorf("默认仓库 = %q", got)
	}
	t.Setenv(RepoEnv, "env/repo")
	if got := (Options{}).repo(); got != "env/repo" {
		t.Errorf("环境变量未生效：%q", got)
	}
	if got := (Options{Repo: "flag/repo"}).repo(); got != "flag/repo" {
		t.Errorf("Options.Repo 优先级应最高：%q", got)
	}
}
