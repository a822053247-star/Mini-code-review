// SPDX-License-Identifier: Apache-2.0

// Package cli 负责参数解析，不执行代码审查。
package cli

// Go 标准库 `flag`，专门用来**解析命令行参数**
import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// Options 保存命令行输入；Git 和模型配置由后续流程解析。
type Options struct {
	Repo       string
	Base       string
	Head       string
	Background string
	Format     string
	Output     string
	Trace      string
	MaxRounds  int
	Mock       bool
}

// Parse 解析并校验参数，将帮助和错误写入 diagnostics。
// 请求帮助时返回 flag.ErrHelp，由调用方决定退出码。
func Parse(args []string, diagnostics io.Writer) (Options, error) {
	var opts Options
	// 独立 FlagSet 避免全局状态；解析失败时返回错误，不直接退出程序。
	flags := flag.NewFlagSet("mini-review", flag.ContinueOnError)
	// 把帮助文本、错误信息都输出到传入的 `diagnostics`（io.Writer），而不是默认 stderr。
	flags.SetOutput(diagnostics)
	// `flags.类型Var(&结构体字段, 参数名, 默认值, 帮助说明)`
	// 含义：把命令行 `-参数名` 的值，绑定到 `opts` 的对应字段。如果用户不填，就用默认值。
	flags.StringVar(&opts.Repo, "repo", ".", "Git repository directory (defaults to the current directory)")
	flags.StringVar(&opts.Base, "base", "HEAD~1", "Base commit or reference")
	flags.StringVar(&opts.Head, "head", "HEAD", "Head commit or reference")
	flags.StringVar(&opts.Background, "background", "", "Requirement or business context")
	flags.StringVar(&opts.Format, "format", "markdown", "Report format: markdown or json")
	flags.StringVar(&opts.Output, "output", "", "Report file path (empty means stdout)")
	flags.StringVar(&opts.Trace, "trace", "", "Optional JSONL trace file path")
	flags.IntVar(&opts.MaxRounds, "max-rounds", 8, "Maximum model requests per file (1 to 20)")
	flags.BoolVar(&opts.Mock, "mock", false, "Use the offline mock client")
	if err := flags.Parse(args); err != nil {
		return Options{}, err
	}
	reject := func(message string) (Options, error) {
		err := fmt.Errorf("%s", message)
		// 把格式化字符串，写入到 diagnostics 这个 io.Writer 对象里面。
		// `fmt.Printf` 固定输出到标准输出 `os.Stdout`
		// `fmt.Fprintf` 可以指定输出到任意地方（这里就是 diagnostics）
		fmt.Fprintf(diagnostics, "Error: %s\n", err)
		return Options{}, err
	}
	if flags.NArg() != 0 {
		return reject("positional arguments are not supported")
	}
	if strings.TrimSpace(opts.Repo) == "" {
		return reject("--repo must not be empty")
	}
	// 拒绝以 '-' 开头的引用，避免后续调用 Git 时被当作命令选项。
	for _, ref := range []struct{ name, value string }{
		{"base", opts.Base}, {"head", opts.Head},
	} {
		if strings.TrimSpace(ref.value) == "" || strings.HasPrefix(ref.value, "-") {
			return reject("--" + ref.name + " must not be empty or start with '-'")
		}
	}
	if opts.Format != "markdown" && opts.Format != "json" {
		return reject("--format must be markdown or json")
	}
	if opts.MaxRounds < 1 || opts.MaxRounds > 20 {
		return reject("--max-rounds must be between 1 and 20")
	}
	// 此处只转为绝对路径，尚不检查目录是否存在或是否为 Git 仓库。
	repo, err := filepath.Abs(opts.Repo)
	if err != nil {
		return reject("cannot resolve --repo to an absolute path: " + err.Error())
	}
	opts.Repo = repo
	return opts, nil
}
