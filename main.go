// clashgo 是 clashctl 的 Go 重写版。
package main

import (
	"os"

	"clashgo/cmd"
)

func main() {
	os.Exit(cmd.Execute())
}
