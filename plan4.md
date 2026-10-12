# 第四天开发计划：Mock、Runner 与离线工具闭环

依据：[需求文档](mini-code-review-agent-requirements.md) 第 5～6、8～11 节、[README](README.md) 当前进度和 [第三天设计记录](notes/day3-design.md)。

工作目录：`E:\study\Mini-code-review`。预计 **4 小时，分四步完成**。

**当天目标：由 Mock 返回工具调用，Runner 执行真实工具、追加完整消息历史并判断完成，离线跑通 `file_read → code_comment → task_done`，同时验证错误恢复与有限停止。**

本文只制定计划，不实施业务代码，也不把下列测试或验收写成已经通过。

## 已有基础与当天边界

复用 `llm.Client/Request/Response/Message`、`model.FileResult/Usage`、`gitdiff.Resolve/Load` 和第三天三个工具及 collector。第三天演示是直接调用注册表；第四天新增的是模型响应驱动的消息循环。

新增 `internal/llm/mock.go`、`internal/agent/runner.go`，按需拆出 `context.go` 与最小 `agent.go`；测试就近放置，另写 `notes/day4-design.md`。不新增通用框架或重复的客户端、工具接口。

当天实现单文件 Runner 和最小逐文件串行调度。完整 Report 状态汇总、Markdown/JSON renderer、JSONL、CLI 装配留到后续；CLI 继续保持已有 help/退出码行为。真实模型适配器与正式英文 prompt 在第五天完善，但今天必须有可运行的英文系统提示词和正确的初始上下文。

## 步骤 1：确定 Runner 契约，实现脚本 Mock（35 分钟）

**准备文件**：`internal/llm/mock.go`、`mock_test.go`、`notes/day4-design.md`。

- 阅读现有客户端、工具 Execution/Completion 和 FileResult 契约；必要时只阅读参考项目 `internal/llmloop/loop.go` 的消息追加与完成判断，不复制整个包。
- Runner 接收 `llm.Client`、绑定当前任务的真实 `*tool.Registry`、初始消息和运行限制；返回当前 `FileResult` 与用量。collector 由装配方持有，停止不清空已验证问题。
- 使用短小的内存响应脚本实现 `llm.Client`：每次 Complete 消费一个 Response 或预设 error，记录收到的 Request，脚本耗尽返回明确英文错误，不循环复用最后一条。
- Mock 检查 context，响应和记录请求保存独立副本，包括 ToolCalls、RawMessage 和 token 指针；复制不能依赖 JSON 往返，因为坏 JSON 参数也需要原样进入 Runner 测试。
- 用显式脚本生成已知 fixture 的 path、新增行和工具调用 ID；Mock 不假装分析任意 Go 代码，不绕过工具校验。阻塞/超时测试使用可控的测试 Client，等待 ctx.Done，不等待真实 60 秒。

**测试与完成标准**：脚本顺序、耗尽、预设错误、取消和副本隔离有测试；确认只实现客户端协议，没有 HTTP、网络或 Key 配置。

## 步骤 2：实现消息循环与同轮完成协议（80 分钟）

**准备文件**：`internal/agent/runner.go`、`context.go` 及对应测试。

**初始上下文**：每文件独立 system/user 消息。system 写固定英文审查规则和工具完成契约；user 包含业务背景、固定 base/head SHA、完整变更路径列表、当前文件带真实新旧行号的 patch 和排序后的 AddedLines。背景、代码、注释用明确数据边界包裹，不拼入 system；读取关联文件不扩大评论权限。这里只复用已加载信息，不重新解析 HEAD。

**每轮顺序**：

1. 检查父 context、轮次和消息预算，调用一次 Client.Complete，记录实际请求次数与返回用量。
2. 验证响应能关联工具结果：角色为 assistant，工具 ID 非空且同轮不重复；不合法的响应不执行工具，记为无效轮次。合法响应先完整追加 assistant，包括原始调用顺序和参数，不改写坏 JSON。
3. 按响应顺序执行全部调用，使用 Registry.Execute；每个普通成功或失败结果序列化成 JSON，追加一个 RoleTool 消息，ToolCallID 精确使用原调用 ID。只序列化 Result，不把程序内部 Completion 写入模型消息。
4. 处理完全部调用才检查完成：本轮有合法 task_done、Completion 指向当前文件，且本轮无工具错误，才返回 completed/task_done。普通文本、零问题和 file_read 成功均不能代替完成声明。

未知工具、非法参数、读取失败等普通错误必须回填，允许下一轮修正。同轮有一次失败即拒绝该轮完成，下一轮需重新调用 task_done；不能沿用旧 Completion。`task_done → code_comment` 仍要收集后续合法评论；普通失败后也继续处理剩余调用。父 context 取消或不可继续的执行错误则立即停止，不为了凑齐回填继续执行。

纯文本回复保留 assistant，再追加简短英文 user 提醒使用工具；不输出“未发现问题”。工具失败已有对应 RoleTool 结果，不伪装成 user 文本。

**核心测试**：读取/评论/完成三轮的下一次 Request 历史、原始参数和每个 ToolCallID；零问题完成；重复评论；同轮完成在前/在后；同轮错误使完成失效并在下一轮恢复；空/重复 ID 不触发工具副作用。

**完成标准**：每个可执行调用都有对应 tool 结果，历史顺序正确，同轮不会漏评论或错误完成。

## 步骤 3：落实停止条件与最小串行调度（75 分钟）

**准备文件**：完善 `runner.go`；按需新增 `internal/agent/agent.go` 与测试，保持装配和循环分离。

| 条件 | 当天约定与验证 |
| --- | --- |
| 无效响应 | 连续两轮无成功工具调用停止为 invalid_response；纯文本、非法响应及全部调用失败统一计数，任一合法成功（含重复评论）重置计数 |
| 最大轮次 | 默认 8，允许 1～20；Rounds 为实际发起的请求数。最后允许轮次仍可完成；未完成才记 max_rounds，不增加补救请求 |
| 消息预算 | 每次请求前累计所有 Message.Content 与 assistant ToolCalls.Arguments 的 UTF-8 字节，最多 64 KiB；恰好上限允许，超出记 context_limit，不截断或压缩历史 |
| 模型请求超时 | 每次派生 60 秒 context，超时记 request_timeout；取消函数在该次调用结束立即释放。测试注入短超时，保留 errors.Is 可识别的 cause |
| 模型调用失败 | 记 model_error；不自动重试，保留此前 collector 内容 |
| Git 子请求超时 | 沿用已有 10 秒约束；工具返回的 deadline Go error 结束当前文件，记 tool_timeout，不当普通 JSON 错误继续请求 |
| 全局取消/父 deadline | 优先检查父 ctx.Err，记 canceled 并向上保留原因；停止剩余调用、请求与文件。后续 CLI 接入时由 signal.NotifyContext 提供 Ctrl+C 取消 |

64 KiB 为需求规定的消息正文与工具参数预算，包含回填的工具 JSON 正文；不等同于 HTTP 序列化体积、Schema 大小或 token 数。若回填后超限且文件尚未完成，在下一次模型请求前停止；本轮已符合完成条件时直接完成，无须额外请求。用多字节字符测试字节口径。

多条件同时命中时优先全局取消；完成判定在同轮调用处理后进行；连续无效停止优先于耗尽轮次。其余检查顺序在设计笔记中固定并测试，避免同一轨迹产生不同 stop_reason。

最小调度接收已经 Load 的 ChangeSet 和 Client，为 Reviewable 文件按路径排序，绑定新 registry 与独立消息历史，共享本次运行 collector；局部模型失败/超时后继续下一文件，父取消则停止。静态跳过不请求模型，无适用文件时请求次数为零。保留 ChangeSet 的跳过与 warning 信息供第六天汇总，不在今天重新实现筛选或最终 Report 状态算法。

统计实际模型请求与工具调用（包括失败、重复和 task_done），复用 model.Usage。token 按输入/输出分别累加：任一次请求该项未知，则总量该项为 nil；已知零保持零，无请求时为已知零，不编造消耗。

**测试与完成标准**：预算 65536/65537 字节、连续无效/成功重置、最后一轮完成、超时/取消、已接受评论保留、逐文件历史隔离、失败继续及取消不调度后续文件均有测试；所有停止路径有限且有明确原因。

## 步骤 4：真实 Git + Mock 离线演示、回归与记录（50 分钟）

**准备文件**：`internal/agent/integration_test.go`、README、`notes/day4-design.md`。

- 用 `t.TempDir()` 建两个提交的 Go 仓库：一个 Reviewable 文件和一个未变更关联文件。保留局部身份、空模板、隔离系统/全局配置、关闭 autocrlf/hooks；跨包不调用 gitdiff/tool 的未导出 helper，不为测试复用增加生产仓库管理器。
- 真实 `Resolve → Load → 最小串行调度 → Runner → Registry`，通过 Mock 三轮完成关联文件读取、当前新增行评论和 task_done。由下一轮 Mock 收到的 Request 验证 tool 结果真正回填，不只断言最后 collector。
- 增加 `TestRunnerOfflineDemo`，英文 t.Logf 输出固定 SHA、轮次、工具名/ID、失败 code、duplicate、collector 数量、文件状态和 stop_reason；演示成功路径及“一轮错误与完成混合、下一轮纠正并重新声明完成”的恢复路径。
- 提交后留下不同暂存/工作区内容，断言仍读取固定 head；工具前后的 status 和 cached diff 一致。需要链接 fixture 时只用索引对象，避免 Windows 链接权限。
- 单元测试覆盖两轮纯文本/全失败停止、无问题完成、多工具同轮、限制和取消；集成测试只负责验证真实装配，不重复第三天全部工具边界。演示不生成最终报告或 trace。

**实施完成后再执行**：

```powershell
Set-Location E:\study\Mini-code-review
$env:GOTOOLCHAIN = 'local'
$env:GOPROXY = 'off'
$env:GOSUMDB = 'off'
gofmt -w .\internal\agent .\internal\llm
go test ./...
go build ./...
go test ./internal/agent -run '^TestRunnerOfflineDemo$' -v
go test ./internal/tool -run '^TestToolsOfflineDemo$' -v
go test ./internal/gitdiff -run '^TestLoadDisplaysNumberedDiff$' -v
go test ./cmd/mini-review -run '^TestRunExitCodes$' -v
git diff --check
```

新 Go 源码加 SPDX 头并保持 LF；新增标识符、prompt、诊断与测试输出用英文，保留已有中文学习注释。不调用参考仓库 Makefile、不修改参考项目、不发起网络请求、不配置 API Key；Git 写操作仅用于临时 fixture。

README 要区分“工具直接调用”“Mock 驱动的 Runner 闭环”和“CLI/真实模型完整审查”。设计笔记记录初始消息、每轮 assistant/tool 追加、同轮错误与完成规则、预算口径、停止原因和实际命令结果；未完成边界明确列出，不提前勾选。

**完成标准**：离线 Runner 演示、全部测试和构建通过；前三天测试未回归，CLI help/退出码不变。能逐条解释为什么追加每条消息、为什么完成或停止。

## 当天验收与后续交接

- [ ] Mock 实现现有 Client，脚本可控，坏 JSON 参数原样保留，无网络依赖。
- [ ] 初始上下文含固定 SHA、完整路径列表、当前编号 patch 和新增锚点，任务间历史独立。
- [ ] assistant 原始调用先保存，每个普通工具结果使用对应 ToolCallID 回填。
- [ ] 同轮调用全部处理后判断完成；有错误不能完成，后续轮次可以修正。
- [ ] 无效响应、轮次、64 KiB、请求超时及全局取消均可有限停止，保留已验证问题。
- [ ] 文件按路径串行，局部失败继续，全局取消停止，无适用文件不请求模型。
- [ ] 离线演示、回归、构建与文档记录完成，CLI 接入和真实模型未被提前宣称完成。

时间不足时优先完成步骤 1～2 的消息闭环及步骤 3 的核心停止规则，压缩日志展示，未完成测试/调度明确记录；不提前开展 P1。第五天在同一个 Client 接口上接真实适配器并完善 prompt；第六天接报告、trace、完整状态汇总与 CLI；第七天完成端到端样例验收。
