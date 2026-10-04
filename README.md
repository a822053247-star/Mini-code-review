# Mini Code Review Agent

用 Go 编写的个人 Agent 练习项目。需求与第一天计划见 [需求文档](mini-code-review-agent-requirements.md) 和 [plan1.md](plan1.md)。

## 当前进度：第一天

已完成独立 Go 模块、审查结果结构、模型客户端与消息契约、CLI 参数解析及必要测试。当前程序只能解析和校验参数，尚未读取 Git diff、执行工具或调用模型，也不会生成报告和 trace。

`--mock`、`--output`、`--trace` 当前只保存参数；真实模型接入前不需要配置 API Key。

## 构建与运行（PowerShell）

需要 Go 1.26.7 或兼容工具链；后续 Git 输入功能需要 Git。

```powershell
Set-Location E:\study\Mini-code-review
go build -o .\bin\mini-review.exe .\cmd\mini-review

# 帮助写入 stderr，退出码为 0
.\bin\mini-review.exe --help
$LASTEXITCODE

# 参数合法；审查尚未实现，退出码为 1
.\bin\mini-review.exe --repo . --base main --head HEAD --format json --mock
$LASTEXITCODE

# 参数错误，退出码为 2
.\bin\mini-review.exe --max-rounds 21
$LASTEXITCODE
```

合法请求当前输出 `Review execution is not implemented yet.`，写入 stderr。所有上述场景的 stdout 均为空；解析成功不代表审查完成。使用编译后的程序查看退出码，避免 `go run` 对退出码的包装。

| 参数 | 默认值 | 当前行为 |
| --- | --- | --- |
| `--repo` | 当前目录 | 转为绝对路径，暂不检查仓库 |
| `--base` | `HEAD~1` | 非空且不以 `-` 开头 |
| `--head` | `HEAD` | 非空且不以 `-` 开头 |
| `--background` | 空 | 原样保存 |
| `--format` | `markdown` | 接受 markdown/json |
| `--output` | 空 | 保存报告路径 |
| `--trace` | 空 | 保存 trace 路径 |
| `--max-rounds` | `8` | 接受 1～20 |
| `--mock` | `false` | 保存 Mock 开关 |

未知参数和位置参数均被拒绝。输出文件可写性、Git 引用解析和模型配置校验将在对应功能实现时加入。

## 格式化与验证

项目目前没有 Makefile，使用自身 Go 工具链检查，不调用参考项目的 Makefile。

```powershell
gofmt -w .\cmd .\internal
go test ./...
go build ./...
```

测试覆盖参数默认值与显式值、路径空格与绝对化、非法输入、轮次边界、help、退出码，以及 JSON 字段、空集合、可空 token 和多工具调用消息。

设计笔记、验证结果和后续任务见 [第一天记录](notes/day1-design.md)。`options.go` 按学习偏好保留中文注释，新增测试使用英文；Go 源码保持 LF 换行。
