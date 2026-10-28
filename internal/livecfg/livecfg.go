// Package livecfg 以只读方式解析已经生效的 runtime.yaml。
//
// 和 cfg 包的分工：cfg 负责「合并并保留注释/顺序」，livecfg 只关心
// 「当前运行时的关键取值」（端口、密钥、TUN 状态等），用结构体解析，
// 简单直接。two 者都读同一份 runtime.yaml，但目的不同。
package livecfg

import (
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Live 是 runtime.yaml 里 clashgo 会用到的字段。
type Live struct {
	MixedPort          int      `yaml:"mixed-port"`
	Port               int      `yaml:"port"`
	SocksPort          int      `yaml:"socks-port"`
	ExternalController string   `yaml:"external-controller"`
	Secret             string   `yaml:"secret"`
	AllowLan           bool     `yaml:"allow-lan"`
	BindAddress        string   `yaml:"bind-address"`
	Authentication     []string `yaml:"authentication"`
	Mode               string   `yaml:"mode"`
	LogLevel           string   `yaml:"log-level"`
	Tun                struct {
		Enable bool `yaml:"enable"`
	} `yaml:"tun"`
	DNS struct {
		Enable bool   `yaml:"enable"`
		Listen string `yaml:"listen"`
	} `yaml:"dns"`
}

// Load 读取并解析 runtime.yaml。
func Load(path string) (*Live, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	var l Live
	if err := yaml.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	return &l, nil
}

// HTTPPort 返回 http 代理应使用的端口：优先 mixed-port，其次 port，最后 7890。
func (l *Live) HTTPPort() int {
	if l.MixedPort > 0 {
		return l.MixedPort
	}
	if l.Port > 0 {
		return l.Port
	}
	return 7890
}

// SocksProxyPort 返回 socks 代理应使用的端口：优先 socks-port，其次 mixed-port。
func (l *Live) SocksProxyPort() int {
	if l.SocksPort > 0 {
		return l.SocksPort
	}
	return l.HTTPPort()
}

// Auth 返回 "user:pass"，没有配置认证时返回空串。
func (l *Live) Auth() string {
	if len(l.Authentication) > 0 {
		return l.Authentication[0]
	}
	return ""
}

// APIPort 从 external-controller 里取端口，缺省 9090。
func (l *Live) APIPort() int {
	addr := l.ExternalController
	if addr == "" {
		return 9090
	}
	i := len(addr) - 1
	for i >= 0 && addr[i] != ':' {
		i--
	}
	if i < 0 {
		return 9090
	}
	if n, err := strconv.Atoi(addr[i+1:]); err == nil {
		return n
	}
	return 9090
}

// TunEnabled 报告 TUN 模式是否启用。
func (l *Live) TunEnabled() bool { return l.Tun.Enable }

// LocalBind 返回本机访问代理时应该用的地址：
// allow-lan 为 false 时是 127.0.0.1，为 true 时按 bind-address 决定
// （`*` 时退回 127.0.0.1，因为普通用户不该把代理地址指向自己不知道的网卡）。
func (l *Live) LocalBind() string {
	if !l.AllowLan {
		return "127.0.0.1"
	}
	switch l.BindAddress {
	case "", "*", "0.0.0.0", "::":
		return "127.0.0.1"
	}
	return l.BindAddress
}
