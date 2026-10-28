package sub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clashgo/internal/env"
	"clashgo/internal/kernel"
)

// validProfile 是一份内核能通过的极简订阅。
const validProfile = `mixed-port: 7890
mode: rule
proxies:
  - name: node-a
    type: ss
    server: 1.2.3.4
    port: 8388
    cipher: aes-256-gcm
    password: pw
proxy-groups:
  - name: PROXY
    type: select
    proxies: [node-a]
rules:
  - MATCH,PROXY
`

const mixinYAML = `mixed-port: 7890
_rules_note: placeholder
rules:
  prefix:
    - DOMAIN,api64.ipify.org,DIRECT
`

// setup 搭一个临时数据面。内核的 Validate/Apply 换成内存桩：
// 真跑 mihomo -t 和 systemctl reload 既慢又需要真实环境，单测不该依赖它们。
func setup(t *testing.T) (*Manager, *env.Env, *recorder) {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"resources/profiles", "bin"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "resources", "mixin.yaml"), []byte(mixinYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(root, ".env")
	content := "KERNEL_NAME=mihomo\nCLASH_BASE_DIR=" + root + "\nINIT_TYPE=systemd\nCLASH_SUB_UA=test-agent\n"
	if err := os.WriteFile(envFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLASHGO_ENV", envFile)
	e, err := env.Load()
	if err != nil {
		t.Fatalf("加载 .env: %v", err)
	}
	m := New(e, kernel.New(e))
	r := &recorder{}
	m.Validate = func(_ context.Context, _ string) error { return nil }
	m.Apply = r.Apply
	return m, e, r
}

// recorder 记录 Apply 收到的配置。
type recorder struct {
	applied [][]byte
}

func (r *recorder) Apply(_ context.Context, data []byte) (bool, error) {
	r.applied = append(r.applied, data)
	return true, nil
}

// TestAddLocalSubscription 从本地文件添加订阅：落盘 profile + 写清单。
func TestAddLocalSubscription(t *testing.T) {
	m, e, _ := setup(t)

	src := filepath.Join(t.TempDir(), "sub.yaml")
	if err := os.WriteFile(src, []byte(validProfile), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := m.Add(context.Background(), src, false)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if p.ID != 1 {
		t.Errorf("首个订阅 id 应为 1，得到 %d", p.ID)
	}
	data, rerr := os.ReadFile(e.ProfilePath(1))
	if rerr != nil {
		t.Fatalf("订阅文件应已落盘: %v", rerr)
	}
	if string(data) != validProfile {
		t.Errorf("落盘内容与源不一致")
	}

	store, _, err := m.Store()
	if err != nil {
		t.Fatal(err)
	}
	if len(store.Profiles) != 1 || store.Profiles[0].URL != src {
		t.Errorf("清单未写入: %+v", store)
	}
}

// TestAddRejectsDuplicate 重复链接在写盘之前就被拒绝。
func TestAddRejectsDuplicate(t *testing.T) {
	m, _, _ := setup(t)
	src := filepath.Join(t.TempDir(), "sub.yaml")
	if err := os.WriteFile(src, []byte(validProfile), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(context.Background(), src, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(context.Background(), src, false); err == nil {
		t.Fatal("重复链接应报错")
	}
}

// TestAddRejectsEmptySubscription 空内容必须被拒绝，不能写出空 profile。
func TestAddRejectsEmptySubscription(t *testing.T) {
	m, e, _ := setup(t)
	src := filepath.Join(t.TempDir(), "empty.yaml")
	if err := os.WriteFile(src, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(context.Background(), src, false); err == nil {
		t.Fatal("空订阅应报错")
	}
	if _, err := os.Stat(e.ProfilePath(1)); !os.IsNotExist(err) {
		t.Error("失败时不应留下 profile 文件")
	}
}

// TestAddFromHTTPServer 走真实 HTTP 路径，并带上 .env 里配置的 UA。
func TestAddFromHTTPServer(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(validProfile))
	}))
	defer srv.Close()

	m, _, _ := setup(t)
	if _, err := m.Add(context.Background(), srv.URL, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if gotUA != "test-agent" {
		t.Errorf("User-Agent = %q，期望 test-agent（来自 .env 的 CLASH_SUB_UA）", gotUA)
	}
}

// TestAddHTTPError 订阅端返回非 2xx 要报错，不能把错误页当成订阅存下来。
func TestAddHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream down"))
	}))
	defer srv.Close()

	m, _, _ := setup(t)
	if _, err := m.Add(context.Background(), srv.URL, false); err == nil {
		t.Fatal("HTTP 502 应报错")
	}
}

// TestUseSwitchesActiveProfile 切换订阅：写 config.yaml → 合并 → 交给内核，
// 并更新清单里的 use。
func TestUseSwitchesActiveProfile(t *testing.T) {
	m, e, rec := setup(t)
	src := filepath.Join(t.TempDir(), "sub.yaml")
	if err := os.WriteFile(src, []byte(validProfile), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := m.Add(context.Background(), src, false)
	if err != nil {
		t.Fatal(err)
	}

	if err := m.Use(context.Background(), p.ID); err != nil {
		t.Fatalf("Use: %v", err)
	}

	cfgData, rerr := os.ReadFile(e.ConfigBase())
	if rerr != nil {
		t.Fatalf("config.yaml 应已写入: %v", rerr)
	}
	if string(cfgData) != validProfile {
		t.Error("config.yaml 内容应与订阅一致")
	}
	if len(rec.applied) != 1 {
		t.Fatalf("应调用一次内核 Apply，实际 %d 次", len(rec.applied))
	}
	if !strings.Contains(string(rec.applied[0]), "api64.ipify.org") {
		t.Errorf("合并结果应包含 mixin 的 rules.prefix，实际:\n%s", rec.applied[0])
	}

	store, _, err := m.Store()
	if err != nil {
		t.Fatal(err)
	}
	if store.Use != p.ID {
		t.Errorf("清单 use 应为 %d，实际 %d", p.ID, store.Use)
	}
}

// TestUseMissingID 切换不存在的订阅要报错。
func TestUseMissingID(t *testing.T) {
	m, _, _ := setup(t)
	if err := m.Use(context.Background(), 42); err == nil {
		t.Fatal("切换不存在的 id 应报错")
	}
}

// TestDeleteRejectsInUse 正在使用的订阅不允许删除。
func TestDeleteRejectsInUse(t *testing.T) {
	m, _, _ := setup(t)
	src := filepath.Join(t.TempDir(), "sub.yaml")
	if err := os.WriteFile(src, []byte(validProfile), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := m.Add(context.Background(), src, false)
	if err != nil {
		t.Fatal(err)
	}
	store, sp, _ := m.Store()
	store.Use = p.ID
	if err := store.Save(sp); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Delete(p.ID); err == nil {
		t.Fatal("删除使用中的订阅应报错")
	}
}

// TestDeleteRemovesFile 删除订阅同时删掉 profile 文件。
func TestDeleteRemovesFile(t *testing.T) {
	m, e, _ := setup(t)
	src := filepath.Join(t.TempDir(), "sub.yaml")
	if err := os.WriteFile(src, []byte(validProfile), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := m.Add(context.Background(), src, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Delete(p.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(e.ProfilePath(p.ID)); !os.IsNotExist(err) {
		t.Error("profile 文件应被删除")
	}
	store, _, _ := m.Store()
	if len(store.Profiles) != 0 {
		t.Errorf("清单应为空，实际 %+v", store.Profiles)
	}
}

// TestUpdateNonActiveDoesNotApply 更新非当前订阅时不应触碰内核。
func TestUpdateNonActiveDoesNotApply(t *testing.T) {
	m, _, rec := setup(t)
	dir := t.TempDir()
	src1 := filepath.Join(dir, "a.yaml")
	src2 := filepath.Join(dir, "b.yaml")
	for _, f := range []string{src1, src2} {
		if err := os.WriteFile(f, []byte(validProfile), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p1, err := m.Add(context.Background(), src1, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(context.Background(), src2, false); err != nil {
		t.Fatal(err)
	}
	store, sp, _ := m.Store()
	store.Use = p1.ID
	if err := store.Save(sp); err != nil {
		t.Fatal(err)
	}

	switched, err := m.Update(context.Background(), 2, false)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if switched || len(rec.applied) != 0 {
		t.Errorf("更新非当前订阅不应触发生效，switched=%v applied=%d", switched, len(rec.applied))
	}
}

// TestUpdateActiveApplies 更新当前订阅必须生效。
func TestUpdateActiveApplies(t *testing.T) {
	m, _, rec := setup(t)
	src := filepath.Join(t.TempDir(), "a.yaml")
	if err := os.WriteFile(src, []byte(validProfile), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := m.Add(context.Background(), src, false)
	if err != nil {
		t.Fatal(err)
	}
	store, sp, _ := m.Store()
	store.Use = p.ID
	if err := store.Save(sp); err != nil {
		t.Fatal(err)
	}

	switched, err := m.Update(context.Background(), p.ID, false)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !switched || len(rec.applied) != 1 {
		t.Errorf("更新当前订阅应生效一次，switched=%v applied=%d", switched, len(rec.applied))
	}
}

// TestReadServerPort 从 pref.yml 抠端口。
func TestReadServerPort(t *testing.T) {
	cases := map[string]int{
		"server:\n  port: 25500\nlog:\n  level: info\n": 25500,
		"server:\n  ip: 127.0.0.1\n  port: 1234\n":      1234,
		"log:\n  port: 1\n":                             0,
		"":                                              0,
	}
	for text, want := range cases {
		if got := readServerPort(text); got != want {
			t.Errorf("readServerPort(%q) = %d，期望 %d", text, got, want)
		}
	}
}
