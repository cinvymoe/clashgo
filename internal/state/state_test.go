package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testPath(t *testing.T) Path {
	t.Helper()
	dir := t.TempDir()
	return Path{
		Meta:     filepath.Join(dir, "profiles.yaml"),
		ProfileF: func(id int) string { return filepath.Join(dir, "profiles", itoa(id)+".yaml") },
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestLoadMissingFile 清单不存在时返回空 Store，而不是报错。
func TestLoadMissingFile(t *testing.T) {
	s, err := Load(testPath(t))
	if err != nil {
		t.Fatalf("清单不存在不应报错: %v", err)
	}
	if len(s.Profiles) != 0 || s.Use != 0 {
		t.Errorf("应为空清单，得到 %+v", s)
	}
}

// TestLoadDerivesPath 读取时 path 必须按 id 重新派生，忽略文件里存的旧路径。
func TestLoadDerivesPath(t *testing.T) {
	p := testPath(t)
	content := `use: 7
profiles:
  - id: 7
    path: /old/installation/resources/profiles/7.yaml
    url: https://example.com/sub
`
	if err := os.WriteFile(p.Meta, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := p.ProfileF(7)
	if s.Profiles[0].Path != want {
		t.Errorf("path 应重新派生为 %q，得到 %q", want, s.Profiles[0].Path)
	}
}

// TestSaveRoundTrip 写回再读出来，语义必须一致，且格式与 bash 版对齐。
func TestSaveRoundTrip(t *testing.T) {
	p := testPath(t)
	s := &Store{Use: 7, Profiles: []Profile{
		{ID: 7, URL: "https://example.com/a"},
		{ID: 9, URL: "https://example.com/b"},
	}}
	if err := s.Save(p); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(p.Meta)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"# 当前使用的订阅", "use: 7", "# 订阅列表", "profiles:", "url: https://example.com/a"} {
		if !strings.Contains(text, want) {
			t.Errorf("写出的清单缺少 %q，实际内容:\n%s", want, text)
		}
	}
	// path 必须写进文件，方便其它工具读
	if !strings.Contains(text, "path: "+p.ProfileF(7)) {
		t.Errorf("应写入派生的 path，实际内容:\n%s", text)
	}

	back, err := Load(p)
	if err != nil {
		t.Fatalf("重新 Load: %v", err)
	}
	if back.Use != 7 || len(back.Profiles) != 2 || back.Profiles[1].ID != 9 {
		t.Errorf("往返后语义不一致: %+v", back)
	}
}

// TestSaveWithoutUse 没有选中订阅时不写 use 字段。
func TestSaveWithoutUse(t *testing.T) {
	p := testPath(t)
	s := &Store{Profiles: []Profile{{ID: 1, URL: "https://example.com/a"}}}
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p.Meta)
	if strings.Contains(string(data), "use:") {
		t.Errorf("Use 为 0 时不应写出 use 字段:\n%s", data)
	}
}

// TestNextID id 分配规则：现有最大值 +1，空清单从 1 开始。
func TestNextID(t *testing.T) {
	if got := (&Store{}).NextID(); got != 1 {
		t.Errorf("空清单 NextID = %d，期望 1", got)
	}
	s := &Store{Profiles: []Profile{{ID: 3}, {ID: 11}, {ID: 5}}}
	if got := s.NextID(); got != 12 {
		t.Errorf("NextID = %d，期望 12", got)
	}
}

// TestAddRejectsDuplicateURL 重复链接必须拒绝。
func TestAddRejectsDuplicateURL(t *testing.T) {
	s := &Store{}
	if _, err := s.Add("https://example.com/a"); err != nil {
		t.Fatalf("首次添加应成功: %v", err)
	}
	if _, err := s.Add("https://example.com/a"); err == nil {
		t.Fatal("重复链接应报错")
	}
	if len(s.Profiles) != 1 {
		t.Errorf("重复添加不应产生新条目，实际 %d 条", len(s.Profiles))
	}
}

// TestRemoveInUse 正在使用的订阅不允许删除。
func TestRemoveInUse(t *testing.T) {
	s := &Store{Use: 7, Profiles: []Profile{{ID: 7}, {ID: 9}}}
	if _, err := s.Remove(7); err == nil {
		t.Fatal("删除使用中的订阅应报错")
	}
	if len(s.Profiles) != 2 {
		t.Errorf("失败时不应改变清单，实际 %d 条", len(s.Profiles))
	}
}

// TestRemoveOK 删除空闲订阅，其余条目顺序保持不变。
func TestRemoveOK(t *testing.T) {
	s := &Store{Use: 7, Profiles: []Profile{{ID: 7}, {ID: 9}, {ID: 11}}}
	p, err := s.Remove(9)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if p.ID != 9 {
		t.Errorf("返回的应是 id=9，得到 %d", p.ID)
	}
	if len(s.Profiles) != 2 || s.Profiles[0].ID != 7 || s.Profiles[1].ID != 11 {
		t.Errorf("剩余条目应为 [7 11]，实际 %+v", s.Profiles)
	}
}

// TestRemoveMissing 删除不存在的 id 报错。
func TestRemoveMissing(t *testing.T) {
	s := &Store{Profiles: []Profile{{ID: 7}}}
	if _, err := s.Remove(42); err == nil {
		t.Fatal("删除不存在的 id 应报错")
	}
}
