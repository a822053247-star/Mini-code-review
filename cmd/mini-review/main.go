// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"example.com/mini-code-review/internal/cli"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, diagnostics io.Writer) int {
	_, err := cli.Parse(args, diagnostics)
	// 程序打印帮助文档，然后退出。返回0是正常退出，1是错误退出。
	// 2是解析参数错误。
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return 2
	}
	fmt.Fprintln(diagnostics, "Review execution is not implemented yet.")
	return 1
}
