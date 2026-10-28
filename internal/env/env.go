// Package env 负责解析 clashctl 的 .env 并推导出所有路径。
//
// 设计要点：路径全部由 BaseDir 派生，不读 profiles.yaml 里存的绝对路径，
// 这样安装目录搬家后不会失效。
package env

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultBaseDir 是 bash 版 clashctl 的默认安装目录。
const DefaultBaseDir = "/data/project/clashctl"

// Env 是 .env 的解析结果加上派生路径。
type Env struct {
	// 来自 .env 的原始值
	BaseDir         string
	KernelName      string
	ConfigURL       string
	SubUA           string
	InitType        string
	ZipUI           string
	URLGhProxy      string
	URLClashUI      string
	VersionMihomo   string
	VersionYq       string
	VersionSubconv  string
	Raw             map[string]string
	EnvFilePath     string
	BaseDirExplicit bool
}

// 派生路径
func (e *Env) ResourcesDir() string  { return filepath.Join(e.BaseDir, "resources") }
func (e *Env) ConfigBase() string    { return filepath.Join(e.ResourcesDir(), "config.yaml") }
func (e *Env) ConfigMixin() string   { return filepath.Join(e.ResourcesDir(), "mixin.yaml") }
func (e *Env) ConfigRuntime() string { return filepath.Join(e.ResourcesDir(), "runtime.yaml") }
func (e *Env) ProfilesDir() string   { return filepath.Join(e.ResourcesDir(), "profiles") }
func (e *Env) ProfilesMeta() string  { return filepath.Join(e.ResourcesDir(), "profiles.yaml") }
func (e *Env) ProfilesLog() string   { return filepath.Join(e.ResourcesDir(), "profiles.log") }
func (e *Env) DistDir() string       { return filepath.Join(e.ResourcesDir(), "dist") }
func (e *Env) BinDir() string        { return filepath.Join(e.BaseDir, "bin") }
func (e *Env) KernelPath() string    { return filepath.Join(e.BinDir(), e.KernelName) }
func (e *Env) SubconvDir() string    { return filepath.Join(e.BinDir(), "subconverter") }
func (e *Env) SubconvPath() string   { return filepath.Join(e.SubconvDir(), "subconverter") }
func (e *Env) SubconvLog() string    { return filepath.Join(e.SubconvDir(), "latest.log") }
func (e *Env) SubconvPref() string   { return filepath.Join(e.SubconvDir(), "pref.yml") }

// TempConfig 是下载订阅时的落地文件（bash 版叫 temp.yaml）。
func (e *Env) TempConfig() string { return filepath.Join(e.ResourcesDir(), "temp.yaml") }

// ZipUIPath 是安装包里自带的 zashboard 压缩包（.env 里的相对路径）。
func (e *Env) ZipUIPath() string {
	if e.ZipUI == "" || filepath.IsAbs(e.ZipUI) {
		return e.ZipUI
	}
	return filepath.Join(e.BaseDir, e.ZipUI)
}

// ProfilePath 按 id 派生订阅文件路径，而不是读 profiles.yaml 里存的 path。
func (e *Env) ProfilePath(id int) string {
	return filepath.Join(e.ProfilesDir(), fmt.Sprintf("%d.yaml", id))
}

// Load 定位并解析 .env。
//
// 查找顺序：
//  1. CLASHGO_ENV 环境变量指定的 .env 路径
//  2. CLASHGO_BASE_DIR 环境变量指定的安装目录下的 .env
//  3. DefaultBaseDir 下的 .env
func Load() (*Env, error) {
	var envPath string
	switch {
	case os.Getenv("CLASHGO_ENV") != "":
		envPath = os.Getenv("CLASHGO_ENV")
	case os.Getenv("CLASHGO_BASE_DIR") != "":
		envPath = filepath.Join(os.Getenv("CLASHGO_BASE_DIR"), ".env")
	default:
		envPath = filepath.Join(DefaultBaseDir, ".env")
	}

	e := &Env{Raw: map[string]string{}, EnvFilePath: envPath}
	if err := e.parseFile(envPath); err != nil {
		return nil, err
	}
	if e.BaseDir == "" {
		if bd := os.Getenv("CLASHGO_BASE_DIR"); bd != "" {
			e.BaseDir = bd
			e.BaseDirExplicit = true
		} else {
			e.BaseDir = DefaultBaseDir
		}
	}
	if e.KernelName == "" {
		e.KernelName = "mihomo"
	}
	return e, nil
}

func (e *Env) parseFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("读取 %s 失败：%w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		e.Raw[key] = val
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("读取 %s 失败：%w", path, err)
	}

	e.BaseDir = e.Raw["CLASH_BASE_DIR"]
	e.KernelName = e.Raw["KERNEL_NAME"]
	e.ConfigURL = e.Raw["CLASH_CONFIG_URL"]
	e.SubUA = e.Raw["CLASH_SUB_UA"]
	e.InitType = e.Raw["INIT_TYPE"]
	e.ZipUI = e.Raw["ZIP_UI"]
	e.URLGhProxy = e.Raw["URL_GH_PROXY"]
	e.URLClashUI = e.Raw["URL_CLASH_UI"]
	e.VersionMihomo = e.Raw["VERSION_MIHOMO"]
	e.VersionYq = e.Raw["VERSION_YQ"]
	e.VersionSubconv = e.Raw["VERSION_SUBCONVERTER"]
	return nil
}

// CheckLayout 确认关键目录存在，给出可读的错误。
func (e *Env) CheckLayout() error {
	for _, d := range []string{e.ResourcesDir(), e.BinDir()} {
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			return fmt.Errorf("安装目录不完整：%s 不存在", d)
		}
	}
	if _, err := os.Stat(e.KernelPath()); err != nil {
		return fmt.Errorf("找不到内核：%s", e.KernelPath())
	}
	return nil
}
