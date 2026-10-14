// SPDX-License-Identifier: Apache-2.0
// 程序入口
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"

	"example.com/mini-code-review/internal/cli"
)

func main() {
	os.Exit(mainExit())
}

func mainExit() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runCommand(ctx, os.Args[1:], os.Stdout, os.Stderr, nil)
}

func run(args []string, diagnostics io.Writer) int {
	return runCommand(context.Background(), args, os.Stdout, diagnostics, nil)
}

func parseOptions(args []string, diagnostics io.Writer) (cli.Options, int) {
	options, err := cli.Parse(args, diagnostics)
	// 程序打印帮助文档，然后退出。返回0是正常退出，1是错误退出。
	// 2是解析参数错误。
	if errors.Is(err, flag.ErrHelp) {
		return cli.Options{}, 0
	}
	if err != nil {
		return cli.Options{}, 2
	}
	return options, -1
}
