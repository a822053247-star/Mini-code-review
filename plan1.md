# 第一天开发计划：项目骨架与接口契约

依据：[Mini Code Review Agent 需求文档](mini-code-review-agent-requirements.md) 第 4、6、8、9、11 节。

工作目录：`E:\study\Mini-code-review`。参考源码目录：`E:\study\alibaba`。

**当天目标：用约 3 小时完成设计梳理、独立 Go 模块、参数解析、Finding/Report 和模型客户端接口。第一步不写代码。**

本计划分为 6 步，按顺序执行。第一天不实现 Git diff、工具、Runner 循环、真实 HTTP 请求或报告渲染；这些分别在后续日期完成。现在只制定计划，按本计划开发时再创建源码。

## 步骤 1：阅读核心链路，梳理消息闭环（40 分钟，不写代码）

**阅读内容**

- 需求文档第 2 节架构、第 6 节执行协议、第 7 节工具契约。
- `E:\study\alibaba\cmd\opencodereview\review_cmd.go`：定位 `executeReviewContext`，观察配置、客户端、工具和 Agent 如何装配。
- `E:\study\alibaba\internal\agent\agent.go`：定位 `New` 和 `Run`，理解编排层与 Runner 的分工。
- `E:\study\alibaba\internal\llmloop\loop.go`：定位 `RunMainTask`、`executeToolCall` 和 `addNextMessage`，关注消息追加与结束判断。
- `E:\study\alibaba\internal\llm\client.go`：只阅读 `LLMClient`、`Message`、`ToolCall` 相关定义，不通读供应商实现。

**操作与产物**

在纸上或 `notes/day1-design.md` 中记录模块职责，并自行画出以下消息闭环。此步骤只阅读、画图和写设计笔记，不创建 `.go` 文件、不初始化模块、不实现函数。

```text
system + user（背景与 patch）
    → 请求模型
    → 保存 assistant（包含 tool_calls）
    → 程序执行工具
    → 追加 tool（携带对应 tool_call_id，内容为结果或错误）
    → 带完整消息历史再次请求模型
    → task_done 且本轮无工具错误，或达到停止条件
```

**完成标准**

能自己解释三个问题：为什么不能只请求一次模型；为什么 tool 结果需要关联调用 ID；为什么 `task_done` 不能覆盖同轮工具错误。标出 Mini 将保留的职责，以及原项目并发、压缩、恢复等暂不实现的能力。

## 步骤 2：建立独立 Go 模块和最小目录（15 分钟）

**准备修改**

- `go.mod`
- `.gitignore`（仅在需要忽略构建产物时添加）
- 后续步骤使用的 `cmd/mini-review/`、`internal/cli/`、`internal/model/`、`internal/llm/` 目录。

**实施内容**

1. 检查本机 Go 和 Git 可用，确认当前目录是 `E:\study\Mini-code-review`。
2. 在现有项目目录初始化独立模块，练习模块名可用 `example.com/mini-code-review`；已有 Git 仓库直接保留，不重新初始化。
3. 使用本机安装的 Go 工具链生成模块版本声明，第一天只依赖标准库。
4. 只建立当天需要的文件，不预建工具、日志、Git 和 Runner 的空实现。

**完成标准**

`go.mod` 属于 Mini 项目；后续文件通过自身模块路径相互引用，不尝试导入 `github.com/alibaba/open-code-review/internal/...`。

## 步骤 3：定义审查结果数据结构（30 分钟）

**准备编写**：`internal/model/model.go`。

| 类型 | 当天定义的字段 | 设计要求 |
| --- | --- | --- |
| `Finding` | Path、StartLine、EndLine、Severity、Category、Title、Description、Suggestion | JSON 字段与需求第 7.2 节一致，行号为整数 |
| `FileResult` | Path、Status、StopReason、Rounds | 独立表示每个文件的完成情况 |
| `Usage` | LLMRequests、ToolCalls、InputTokens、OutputTokens | token 用 `*int64` 表示未知；未知输出为 `null` |
| `Report` | Status、BaseSHA、HeadSHA、Files、Findings、Warnings、Usage、DurationMS | JSON 字段与需求第 8.1 节一致 |

**实施内容**

- 给上述结构添加明确 JSON tag，warnings 暂用 `[]string`，文件与问题使用具体类型的 slice。
- 定义运行状态 `completed`、`skipped`、`partial`、`failed`；文件状态先定义 `completed`、`skipped`、`failed`，未完成原因通过 StopReason 表达。
- 定义严重程度 `high`、`medium`、`low` 和类别 `bug`、`security`、`performance` 的常量。
- 保持数据层只存储契约，不在这里实现评论校验、去重、状态汇总或渲染。
- 约定将来创建报告时初始化空 slice，确保空集合输出 `[]`；token 未知仍输出 `null`。

**完成标准**

能用需求中的 JSON 示例逐项对应字段；能说明“未知 token”与“零 token”、运行状态与单文件状态的区别。

## 步骤 4：定义模型客户端与消息契约（25 分钟）

**准备编写**：`internal/llm/client.go`。

**实施内容**

- 定义 `Client` 接口，仅提供一个接收 `context.Context` 和 Request、返回 Response 与 error 的方法，方法名可用 `Complete`。
- 定义 `Request`：Messages、Tools；模型名称和服务配置留给具体客户端，不在每个调用点重复传递。
- 定义 `Message`：Role、Content、ToolCalls、ToolCallID；P0 的 Content 使用 string，不兼容多模态内容块。
- 定义 `ToolCall`：ID、Name、Arguments；Arguments 使用 `json.RawMessage` 保留原始 JSON，具体工具执行时再解析和校验。
- 定义 `ToolDefinition`：Name、Description、Parameters；Parameters 用 `json.RawMessage` 存放 JSON Schema。
- 定义 `Response`：Assistant、可选 Usage；Assistant 用 Message 表达，避免重复存储同一组工具调用。
- 定义客户端返回的 token 用量类型，缺失用指针或可空字段表达；运行累计仍由后续编排层负责。

这些是 Mini 内部类型，不要求直接等同于某个服务的 HTTP JSON。真实适配器负责把内部类型映射到服务协议。

**完成标准**

能够表示一条携带多个 tool_calls 的 assistant 消息，以及每个调用的 tool 结果；客户端接口支持取消，且不依赖 Agent、工具实现或 API Key。第一天不实现真实客户端和正式 Mock 脚本。

## 步骤 5：实现参数解析和可运行入口（45 分钟）

**准备编写**：`internal/cli/options.go`、`cmd/mini-review/main.go`。

**参数契约**

| 参数 | 默认值 | 当天校验 |
| --- | --- | --- |
| `--repo` | 当前目录 | 非空，转换为绝对路径；仓库根目录识别留到第 2 天 |
| `--base` | `HEAD~1` | 非空，不以 `-` 开头 |
| `--head` | `HEAD` | 非空，不以 `-` 开头 |
| `--background` | 空 | 保留原文本 |
| `--format` | `markdown` | 仅允许 markdown/json |
| `--output` | 空 | 保存参数；可写性检查留到输出实现时 |
| `--trace` | 空 | 保存参数；日志文件检查留到日志实现时 |
| `--max-rounds` | `8` | 仅允许 1～20 |
| `--mock` | `false` | 保存布尔值；当天不触发模型调用 |

**实施内容**

- 定义 Options，使用独立 `flag.FlagSet` 与 `flag.ContinueOnError` 解析，便于单测，不直接使用全局 flag 状态。
- 解析逻辑接收参数列表和诊断 writer，拒绝未知参数及多余位置参数。
- 解析函数返回 Options 与 error，避免内部调用 `os.Exit`；只有 main 负责进程退出。
- `--help` 展示参数说明并返回 0；参数错误将英文说明写 stderr 并返回 2。
- 合法参数解析后，暂向 stderr 输出英文“Review execution is not implemented yet.”并返回 1。此时未执行审查，不能输出 completed 或伪造审查报告。
- stdout 保持为空；不打印完整配置，尤其不打印 Key。模型环境变量读取与校验留到第 5 天真实适配器接入时完成。

**完成标准**

入口可构建，支持全部需求参数，参数错误与 help 的行为明确。能够解释“CLI 解析成功”并不等于“审查完成”；目前合法请求返回 1 是开发阶段明确的未实现状态。

## 步骤 6：验证契约与 CLI，记录当天成果（25 分钟）

**准备编写**：`internal/cli/options_test.go`、`internal/model/model_test.go`，并补充现有 README 的第一天运行说明。

**必要验证**

- CLI 表驱动测试：默认参数、显式参数、带空格路径、未知 flag、多余位置参数、非法 format、max-rounds 的 0/1/20/21 边界、非法 ref、help。
- model JSON 测试：字段名正确、空集合为 `[]`、未知 token 为 `null`，已知零 token 为 `0`。
- 格式化当天源码，运行独立项目的单元测试与构建。若已建立 Makefile，使用其检查和测试目标；未建立时用 Go 工具链的对应命令，不调用参考仓库的 Makefile。
- 手动运行 help，以及一条带合法参数的命令，检查 stdout/stderr 和退出行为。验证进程退出码时使用构建后的可执行文件，避免 `go run` 包装实际退出码。
- 将步骤 1 的消息闭环笔记与“已完成/待完成”状态保存在项目内；源码中的注释、标识符和提示文本使用英文，文件保持 LF。

**完成标准**

测试和构建通过，README 说明当前只能解析参数；设计笔记能解释完整消息闭环，后续任务有清晰接口可接入。

## 当天交付与结束检查

预计新增源码清单：

```text
Mini-code-review/
├── go.mod
├── cmd/mini-review/main.go
├── internal/cli/options.go
├── internal/cli/options_test.go
├── internal/model/model.go
├── internal/model/model_test.go
├── internal/llm/client.go
└── notes/day1-design.md
```

目录清单是当天开发目标，不表示这些文件已创建。

- [ ] 第一步完成阅读与闭环图，没有先生成实现再回头阅读。
- [ ] 独立 Go 模块可构建，未引入 Agent 框架或模型 SDK。
- [ ] CLI 参数默认值、校验、help 和退出行为符合当天约定。
- [ ] Finding、Report、Message、ToolCall 和 Client 契约已定义。
- [ ] 构建及必要测试通过，未对真实模型发起请求。
- [ ] 能独立解释 assistant/tool 消息关联，以及 task_done 的完成边界。

若时间不足，优先保证步骤 1～5 的核心产物，再完成默认参数、非法参数和 JSON 可空用量的关键验证；不要通过增加空 Agent、假报告或无意义接口来追求文件数量。第 2 天从 Git 输入与固定提交 SHA 开始。
