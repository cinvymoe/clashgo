package netutil

import (
	"os/exec"
	"strings"
)

// runQuiet 跑一条只读命令并返回 stdout；出错返回空。
func runQuiet(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// splitFields 按空白切分（不引入 strings.Fields 的额外 import 语义差异）。
func splitFields(s string) []string { return strings.Fields(s) }
