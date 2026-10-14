# Mini Code Review Agent

用 Go 编写的个人 Agent 练习项目。需求与开发计划见 [需求文档](mini-code-review-agent-requirements.md)、[plan1.md](plan1.md) 和 [plan2.md](plan2.md)。

## 当前进度：第五天真实模型适配器与冒烟验证

已完成独立 Go 模块、审查结果结构、模型客户端与消息契约、CLI 参数解析，以及 `internal/gitdiff` 的固定提交解析、逐文件 patch、head 内容读取、行号映射、筛选和范围限制。

Git 输入层已能在测试中通过 `Resolve → Load` 运行，返回含所有变更文件、审查标记、跳过原因和覆盖警告的 `ChangeSet`。CLI 尚未接入该流程，仍然只解析和校验参数；CLI 的完整审查、工具执行、模型调用、报告和 trace 尚未实现。

第三天已完成 `internal/tool` 的固定注册表、三个工具定义、严格参数解码和统一成功/失败结果，以及三个具体工具。`file_read` 可读取未变更关联 Go 文件，默认 120 行、最多 200 行/16 KiB 带行号正文，返回实际范围和截断标记；与 Load 共享静态筛选规则。`code_comment` 校验当前文件、head 行范围和新增起点，将合法问题按精确去重规则收集；`task_done` 返回单文件完成声明，允许零问题，不改变 collector。真实临时 Git 仓库测试验证固定 head，工具测试验证评论及完成信号隔离。契约与进度见 [第三天记录](notes/day3-design.md)。

第四天已实现脚本 Mock、独立 system/user 初始上下文、单文件 Runner 和最小串行 Agent。离线单测验证多轮消息回填、同轮完成、错误恢复、停止条件及 64 KiB 边界。调度按路径运行独立文件任务，共享本次 collector；局部失败继续，全局取消停止，保留静态跳过、warnings、覆盖缺口和用量。`TestRunnerOfflineDemo` 使用两次提交的真实临时仓库、生产 Git 读取和脚本 Mock，执行完整 `Resolve → Load → Agent → Runner → Registry`，验证三轮成功路径与错误后重新声明完成的恢复路径。实现与实际验证记录见 [第四天记录](notes/day4-design.md)，后续任务见 [plan4.md](plan4.md)。

第五天新增标准库 HTTP 适配器，实现硅基流动非流式 Chat Completions 协议；固定英文 system prompt 独立保存并嵌入程序。2026-10-13 使用 `deepseek-ai/DeepSeek-V4-Flash` 在合成临时仓库完成真实 `file_read → code_comment → task_done`，3 轮、4 次工具调用，文件 completed，一条合法 Finding；工作区/索引保持不变。配置、协议、脱敏错误、超时取消和实际结果见 [第五天记录](notes/day5-design.md)。CLI 仍只解析参数，完整报告、trace、CLI 装配留到第六天。

`TestToolsOfflineDemo` 通过真实 `Resolve → Load` 绑定三个工具，独立执行 `file_read → code_comment → 重复 code_comment → task_done`，并插入失败调用验证 collector 无副作用。演示读取固定 head 中的当前文件和未变更关联文件，检查真实行号、AddedLines、重复标记及单文件完成声明；工具调用前后的 status/cached diff 保持一致。它直接调用注册表，不使用模型或 Mock Client；Runner 闭环另由 TestRunnerOfflineDemo 验证。

| 验证层次 | 当前状态 |
| --- | --- |
| 工具直接调用 | TestToolsOfflineDemo 独立验证三个工具 |
| Mock 驱动的 Agent/Runner 闭环 | TestRunnerOfflineDemo 用下一轮 Request 验证 assistant 原始调用和对应工具 JSON 真正回填，测试失败恢复与最终文件状态 |
| 真实模型驱动的核心审查 | TestAgentLiveSmoke 显式启用后，在合成仓库调用真实适配器、Agent/Runner 与工具；已完成一次真实验证 |
| CLI 完整审查 | 尚未实现；完整 Report 状态汇总、renderer、trace 与 CLI 接入留到后续 |

Runner 演示仅输出英文测试日志，不生成报告或 trace。暂存区/工作区修改及关联文件删除不改变固定 head 的读取，演示前后 status/cached diff 保持一致。Mock 的评论用于协议验证，不代表对任意代码的自动分析结果。

`--mock`、`--output`、`--trace` 当前只保存参数。普通测试和 Mock 不需要 API Key；显式真实冒烟需要模型配置。

## 真实模型配置与显式冒烟（第五天）

只实现一种协议，使用硅基流动 [Chat Completions](https://docs.siliconflow.cn/docs/api/chat-completions-post) 和 [Function Calling](https://docs.siliconflow.cn/docs/userguide/guides/function-calling)。配置来自进程环境，不自动加载 env 文件：

| 环境变量 | 用途 |
| --- | --- |
| `MINI_LLM_ENDPOINT` | 完整请求 URL，例如 `https://api.siliconflow.cn/v1/chat/completions`，不是 `/v1` 基地址 |
| `MINI_LLM_MODEL` | 模型名，例如 `deepseek-ai/DeepSeek-V4-Flash` |
| `MINI_LLM_API_KEY` | Bearer 凭证；不写入仓库或报告 |
| `MINI_REVIEW_LIVE` | 只有显式设为 `1` 才运行真实冒烟；普通 `go test ./...` 跳过该测试 |

```powershell
Set-Location E:\study\Mini-code-review
$env:GOTOOLCHAIN = 'local'
$env:GOPROXY = 'off'
$env:GOSUMDB = 'off'
$env:MINI_LLM_ENDPOINT = 'https://api.siliconflow.cn/v1/chat/completions'
$env:MINI_LLM_MODEL = 'deepseek-ai/DeepSeek-V4-Flash'
$secureKey = Read-Host 'SiliconFlow API key' -AsSecureString
$env:MINI_LLM_API_KEY = [System.Net.NetworkCredential]::new('', $secureKey).Password
$env:MINI_REVIEW_LIVE = '1'
try {
    go test ./internal/agent -run '^TestAgentLiveSmoke$' -v -count=1 -timeout 4m
} finally {
    Remove-Item Env:MINI_LLM_API_KEY, Env:MINI_REVIEW_LIVE -ErrorAction SilentlyContinue
    $secureKey.Dispose()
}
```

真实冒烟只发送测试生成的代码，使用真实工具读取固定 head；60 秒请求超时、3 分钟总保护和 Ctrl+C context 取消，默认不重试。日志只记录调用名称、状态、位置与实际用量，不打印密钥、请求头、原始 HTTP 错误或模型推理。模型问题识别不作为确定性准确率测试；本次评论仅说明该次运行结果。CLI 的 `--mock` 尚未连接这个入口。

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
$env:GOTOOLCHAIN = 'local'
$env:GOPROXY = 'off'
$env:GOSUMDB = 'off'
gofmt -w .\cmd .\internal
go test ./...
go build ./...

# 真实 Git + Mock：成功闭环与同轮失败后的完成恢复
go test ./internal/agent -run '^TestRunnerOfflineDemo$' -v

# 独立工具离线演示：英文日志输出固定 SHA、结果、duplicate、数量和声明
go test ./internal/tool -run '^TestToolsOfflineDemo$' -v

# 从临时仓库的两个真实提交加载并显示带行号 diff
go test ./internal/gitdiff -run '^TestLoadDisplaysNumberedDiff$' -v
go test ./cmd/mini-review -run '^TestRunExitCodes$' -v
git diff --check
```

测试覆盖第一天的参数、help、退出码与消息契约，以及第二天的固定 SHA、仓库子目录、无效 ref、取消、普通修改、新增、空格路径、重命名按删除/新增处理、筛选原因和多 hunk 行号映射。大小边界覆盖 8/9 文件、400/401 新增加删除行、32 KiB patch 和 256 KiB head 内容。

演示测试打印文件路径、base/head SHA、`+ head:N` / `- base:N` / 上下文行号和排序后的 `AddedLines: [4 5]`，并将行号逐项与提交内容核对。测试仓库通过 `t.TempDir()` 创建，隔离全局/系统 Git 配置、关闭自动换行转换和 hooks；符号链接通过索引对象构造，不要求 Windows 链接权限。测试同时验证读取不会改变临时仓库的工作区或索引。

设计笔记、验证结果和后续任务见 [第一天记录](notes/day1-design.md)、[第二天记录](notes/day2-design.md)、[第三天记录](notes/day3-design.md) 和 [第四天记录](notes/day4-design.md)。源码按学习偏好保留已有中文注释，新增标识符和诊断使用英文；Go 源码保持 LF 换行。
