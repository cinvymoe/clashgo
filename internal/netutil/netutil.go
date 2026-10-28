// Package netutil 收拢几个本地端口的小工具。
//
// 单独成包是因为 sub（订阅转换端口）、app（代理端口检测）都要用，
// 放在任一业务包里都别扭。
package netutil

import (
	"fmt"
	"net"
	"strconv"
	"time"
)

// PortInUse 判断本地端口是否已被监听。
// 用 DialTimeout 而不是 bind：bind 会漏掉只监听在特定地址上的服务。
func PortInUse(port int) bool {
	conn, err := net.DialTimeout("tcp",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// FreePort 向系统要一个当前空闲的端口。
func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("分配空闲端口失败: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// LocalIP 取本机在默认路由上的地址。
// 先问 ip route，再退回 hostname -I；都拿不到时返回空串。
func LocalIP() string {
	if out, err := runQuiet("ip", "route", "get", "1.1.1.1"); err == nil {
		fields := splitFields(out)
		for i := 0; i < len(fields)-1; i++ {
			if fields[i] == "src" {
				return fields[i+1]
			}
		}
	}
	if out, err := runQuiet("hostname", "-I"); err == nil {
		fields := splitFields(out)
		if len(fields) > 0 {
			return fields[0]
		}
	}
	return ""
}
