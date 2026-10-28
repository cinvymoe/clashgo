package schedule

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withTempHome 把 HOME 指到临时目录，避免测试污染真实 ~/.config。
func withTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// 清掉 XDG_CONFIG_HOME，否则会绕开 HOME
	t.Setenv("XDG_CONFIG_HOME", "")
	return home
}

func TestParseInterval(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", 6 * time.Hour, false},
		{"6h", 6 * time.Hour, false},
		{"2d", 48 * time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"1h30m", 90 * time.Minute, false},
		{"1m", 0, true},  // 太短
		{"40d", 0, true}, // 太长
		{"abc", 0, true}, // 无法解析
		{"0d", 0, true},  // 0 天不合法
	}
	for _, c := range cases {
		got, err := ParseInterval(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseInterval(%q) 应该报错，却得到 %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseInterval(%q) 意外报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseInterval(%q) = %v, 期望 %v", c.in, got, c.want)
		}
	}
}

func TestCalendarExpr(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
		ok   bool
	}{
		{6 * time.Hour, "*-*-* 00/6:00:00", true},
		{2 * time.Hour, "*-*-* 00/2:00:00", true},
		{1 * time.Hour, "*-*-* 00/1:00:00", true},
		{24 * time.Hour, "*-*-* 00/24:00:00", true},
		{5 * time.Hour, "", false},    // 不能整除 24
		{48 * time.Hour, "", false},   // 超过 24
		{30 * time.Minute, "", false}, // 小于 1 小时
	}
	for _, c := range cases {
		got, ok := calendarExpr(c.d)
		if ok != c.ok {
			t.Errorf("calendarExpr(%v) ok=%v, 期望 %v", c.d, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("calendarExpr(%v) = %q, 期望 %q", c.d, got, c.want)
		}
	}
}

func TestServiceFile(t *testing.T) {
	s := Spec{BaseDir: "/data/project/clashctl", Binary: "/usr/local/bin/clashgo", Interval: 6 * time.Hour}
	out := s.ServiceFile()

	if !strings.HasPrefix(out, Marker) {
		t.Fatalf("service 文件缺少 Marker，Remove 会拒绝删除:\n%s", out)
	}
	for _, want := range []string{
		"[Service]",
		"Type=oneshot",
		"ExecStart=/usr/local/bin/clashgo sub update",
		"Environment=CLASHGO_BASE_DIR=/data/project/clashctl",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("service 文件缺少 %q:\n%s", want, out)
		}
	}
}

func TestServiceFileAll(t *testing.T) {
	s := Spec{Binary: "/usr/local/bin/clashgo", All: true}
	out := s.ServiceFile()
	if !strings.Contains(out, "ExecStart=/usr/local/bin/clashgo sub update --all") {
		t.Errorf("--all 没有写进 ExecStart:\n%s", out)
	}
	// 没给 BaseDir 时不应该有 Environment 行
	if strings.Contains(out, "Environment=") {
		t.Errorf("BaseDir 为空时不该写 Environment:\n%s", out)
	}
}

func TestTimerFileCalendar(t *testing.T) {
	s := Spec{Interval: 6 * time.Hour}
	out := s.TimerFile()

	for _, want := range []string{
		"[Timer]",
		"OnCalendar=*-*-* 00/6:00:00",
		"Persistent=true",          // 关机补跑
		"RandomizedDelaySec=10min", // 打散
		"Unit=clashgo-sub-update.service",
		"WantedBy=timers.target",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("timer 文件缺少 %q:\n%s", want, out)
		}
	}
	// OnCalendar 模式下不该出现 OnUnitActiveSec
	if strings.Contains(out, "OnUnitActiveSec=") {
		t.Errorf("整小时间隔不该用 OnUnitActiveSec:\n%s", out)
	}
}

func TestTimerFileNonCalendar(t *testing.T) {
	s := Spec{Interval: 90 * time.Minute}
	out := s.TimerFile()

	if strings.Contains(out, "OnCalendar=") {
		t.Errorf("90 分钟不是整小时，不该用 OnCalendar:\n%s", out)
	}
	if strings.Contains(out, "Persistent=true") {
		t.Errorf("OnUnitActiveSec 模式下 Persistent 无效，不该写:\n%s", out)
	}
	for _, want := range []string{
		"OnBootSec=5min",
		"OnUnitActiveSec=90min",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("timer 文件缺少 %q:\n%s", want, out)
		}
	}
}

func TestInstallAndRemove(t *testing.T) {
	withTempHome(t)
	s := Spec{BaseDir: "/tmp/base", Binary: "/usr/local/bin/clashgo", Interval: 6 * time.Hour}

	// 直接测文件层，不依赖真实 systemd：
	// Available() 在 CI 里可能为 true 也可能为 false，所以只断言文件结果。
	svcPath, err := ServicePath()
	if err != nil {
		t.Fatal(err)
	}
	tmrPath, err := TimerPath()
	if err != nil {
		t.Fatal(err)
	}

	// 手动写文件（绕过 Install 里的 systemctl）
	if _, err := writeFile(svcPath, s.ServiceFile()); err != nil {
		t.Fatal(err)
	}
	if _, err := writeFile(tmrPath, s.TimerFile()); err != nil {
		t.Fatal(err)
	}

	// 幂等：再写一次应该报告未变化
	changed, err := writeFile(svcPath, s.ServiceFile())
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("内容相同时 writeFile 应返回 changed=false")
	}

	// Current 应该能看到
	st, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Installed {
		t.Error("文件已写入，Installed 应为 true")
	}
	if st.Interval != "*-*-* 00/6:00:00" {
		t.Errorf("Interval 解析错误: %q", st.Interval)
	}

	// Remove 应该删掉
	removed, err := Remove()
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Error("Remove 应该返回 removed=true")
	}
	for _, p := range []string{svcPath, tmrPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s 应该已被删除", p)
		}
	}

	// 再删一次是幂等的
	removed, err = Remove()
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Error("第二次 Remove 应返回 removed=false")
	}
}

// TestRemoveKeepsForeignUnit 确认不会删掉用户手写的同名 unit。
func TestRemoveKeepsForeignUnit(t *testing.T) {
	withTempHome(t)
	svcPath, err := ServicePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(svcPath), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := "[Unit]\nDescription=我自己写的\n\n[Service]\nExecStart=/bin/true\n"
	if err := os.WriteFile(svcPath, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := Remove()
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Error("没有 Marker 的文件不该被删除")
	}
	data, err := os.ReadFile(svcPath)
	if err != nil {
		t.Fatalf("手写的 unit 被删了: %v", err)
	}
	if string(data) != foreign {
		t.Error("手写的 unit 内容被改动了")
	}
}

func TestUnitDuration(t *testing.T) {
	cases := map[time.Duration]string{
		6 * time.Hour:    "6h",
		48 * time.Hour:   "2d",
		90 * time.Minute: "90min",
		30 * time.Second: "30s",
	}
	for d, want := range cases {
		if got := unitDuration(d); got != want {
			t.Errorf("unitDuration(%v) = %q, 期望 %q", d, got, want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		6 * time.Hour:    "6 小时",
		48 * time.Hour:   "2 天",
		30 * time.Minute: "30 分钟",
	}
	for d, want := range cases {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%v) = %q, 期望 %q", d, got, want)
		}
	}
}
