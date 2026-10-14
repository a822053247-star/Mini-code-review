# 第六天离线报告与日志样例

2026-10-14 由 TestCLIDay6Demo 实际运行 CLI 装配生成，fixture 为临时仓库的两个提交，a.go/z.go 各修改一行。普通测试不生成这些文件；测试结束后 fixture 自动删除，SHA 用于记录该次比较。

| 场景 | 报告 | 日志 | 实际结果 |
| --- | --- | --- | --- |
| 正常 | [completed.json](completed.json) | [completed.trace.jsonl](completed.trace.jsonl) | completed；2 条脚本 Finding，6 请求/6 工具调用，退出 0 |
| 不完整 | [partial.md](partial.md) | [partial.trace.jsonl](partial.trace.jsonl) | a.go 完成、z.go 模型失败；partial，2 请求/1 工具调用，退出 1 |

Mock 评论是明确标记的协议例子，不是对源代码的真实结论；不完整样例不会写“审查无问题”。用量未提供时为 null/unknown。trace 与报告状态相同，可按 file/round/tool_call_id 复盘；日志没有代码正文、参数、Key 或推理。

重新生成时选择一个全新的输出目录，现有文件会被拒绝：

```powershell
$env:GOTOOLCHAIN = 'local'
$env:GOPROXY = 'off'
$env:GOSUMDB = 'off'
$env:MINI_REVIEW_DEMO_DIR = Join-Path $env:TEMP ('mini-review-demo-' + [guid]::NewGuid())
go test ./cmd/mini-review -run '^TestCLIDay6Demo$' -v -count=1
```

这两个产物通过实际命令行装配与真实 Git/工具生成；不使用网络模型。Git 写操作仅发生在临时 fixture 仓库。
