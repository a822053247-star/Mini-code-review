# Mini Code Review Agent

用 Go 编写的个人 Agent 练习项目。需求与开发计划见 [需求文档](mini-code-review-agent-requirements.md)、[plan1.md](plan1.md) 和 [plan2.md](plan2.md)。

## 当前进度：第二天 Git 输入层

已完成独立 Go 模块、审查结果结构、模型客户端与消息契约、CLI 参数解析，以及 `internal/gitdiff` 的固定提交解析、逐文件 patch、head 内容读取、行号映射、筛选和范围限制。

Git 输入层已能在测试中通过 `Resolve → Load` 运行，返回含所有变更文件、审查标记、跳过原因和覆盖警告的 `ChangeSet`。CLI 尚未接入该流程，仍然只解析和校验参数；完整审查、工具执行、模型调用、报告和 trace 尚未实现。

`--mock`、`--output`、`--trace` 当前只保存参数；真实模型接入前不需要配置 API Key。

## 构建与运行（PowerShell）

需要 Go 1.26.7 或兼容工具链；Git 输入测试需要 Git。

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

# 从临时仓库的两个真实提交加载并显示带行号 diff
go test ./internal/gitdiff -run '^TestLoadDisplaysNumberedDiff$' -v
```

测试覆盖第一天的参数、help、退出码与消息契约，以及第二天的固定 SHA、仓库子目录、无效 ref、取消、普通修改、新增、空格路径、重命名按删除/新增处理、筛选原因和多 hunk 行号映射。大小边界覆盖 8/9 文件、400/401 新增加删除行、32 KiB patch 和 256 KiB head 内容。

演示测试打印文件路径、base/head SHA、`+ head:N` / `- base:N` / 上下文行号和排序后的 `AddedLines: [4 5]`，并将行号逐项与提交内容核对。测试仓库通过 `t.TempDir()` 创建，隔离全局/系统 Git 配置、关闭自动换行转换和 hooks；符号链接通过索引对象构造，不要求 Windows 链接权限。测试同时验证读取不会改变临时仓库的工作区或索引。

设计笔记、验证结果和后续任务见 [第一天记录](notes/day1-design.md) 和 [第二天记录](notes/day2-design.md)。源码按学习偏好保留已有中文注释，新增标识符和诊断使用英文；Go 源码保持 LF 换行。
