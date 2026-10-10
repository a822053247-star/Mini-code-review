# 第三天开发计划：三个工具、参数校验与 Finding 收集

依据：[README](README.md) 的第二天进度、[需求文档](mini-code-review-agent-requirements.md) 第 6～7 节、第 9～11 节，以及 [第二天设计记录](notes/day2-design.md)。

工作目录：`E:\study\Mini-code-review`。参考项目：`E:\study\alibaba`，只按需阅读，不修改或提交参考代码。

**当天目标：约 4 小时，在不接模型的情况下，通过工具注册表读取固定 head 的代码、校验并收集问题、返回当前文件的完成声明。**

本文仅为计划，以下代码、测试和验证结果尚未实施。第三天不实现 Mock 客户端、Agent、Runner、真实模型、报告 renderer 或 trace；CLI 继续保持现有 help/退出码行为。工具可在测试中运行，不代表完整审查已经完成。

## 已有基础与当天边界

| 现有能力 | 第三天如何使用 |
| --- | --- |
| `gitdiff.Resolve` / `Snapshot` | 任务启动前固定 base/head；工具不重新解析分支 |
| `gitdiff.Load` / `ChangeSet` | 只为 Reviewable 文件建立评论任务，保留所有变更的筛选信息 |
| `gitdiff.ReadHeadFile` | 读取固定 head 的普通 blob，已有路径、文件类型及 256 KiB 保护 |
| `FileChange.HeadContent` / `AddedLines` | 获取当前文件总行数、校验问题锚点 |
| `model.Finding` / Severity / Category | 复用已有结构和枚举，不另建重复 Finding |
| `llm.ToolDefinition` / `ToolCall` | 提供工具定义并保留原始 JSON 参数；不修改模型协议 |

`file_read` 可以读取未变更的关联 Go 文件，但不会扩大 `code_comment` 的当前文件或新增锚点范围。工具负责能力和参数校验；是否接受完成、消息历史与同轮错误处理由第四天 Runner 负责。

## 步骤 1：阅读契约，固定工具输入输出（20 分钟）

**准备产物**：`notes/day3-design.md` 初稿。

**阅读内容**

- 需求第 7 节：三个工具、范围和去重规则。
- 本项目 `internal/gitdiff/{git,load,types}.go`、`internal/model/review.go`、`internal/llm/client.go`。
- 参考项目 `internal/tool/definitions.go`、`file_read.go`、`filereader.go`、`code_comment.go`、`comment_collector.go`；只看注册、读取和收集职责，不搬入框架依赖。

**确定约定**

1. 每个文件任务绑定一个 Snapshot、一个 Reviewable 的 FileChange 和本次运行的 collector；允许评论的 Path/AddedLines 来自程序，不从工具参数更新。
2. 固定三个名称：`file_read`、`code_comment`、`task_done`，各有英文说明与 JSON Schema。
3. 工具结果统一为成功/失败 JSON，例如成功含 `ok: true, data: {...}`，失败含 `ok: false, error: {code, message}`。错误码和诊断使用英文。
4. `task_done` 成功返回独立的完成声明信号与 summary；不直接设置 Report 状态，也不修改 collector。
5. context 取消和超时必须能通过 `errors.Is` 识别并传给调用方；与可修正的参数/工具错误区分，留给 Runner 决定全局停止。

**完成标准**：能说明工具成功、任务完成声明、最终审查完成三者的区别；确定可测试的返回契约后再开始实现。

## 步骤 2：实现固定注册表与严格参数解码（35 分钟）

**准备编写**：`internal/tool/registry.go`、`registry_test.go`。

**实施内容**

- 使用固定 map 分派三个工具；工具定义按稳定顺序返回，复用 `llm.ToolDefinition`。不设计插件系统或通用依赖容器。
- 为每个工具定义明确的参数结构。可选行号要区分“未传入”和“显式传入 0”，不能把非法值当默认值。
- 使用标准库 JSON 解码，拒绝坏 JSON、未知字段、非对象输入、尾随第二个 JSON 值、错误类型，以及必填字段缺失/null。Schema 写 `required` 和 `additionalProperties: false`，程序仍独立校验。
- 未知工具返回稳定错误结果，不能 panic；失败没有评论或完成副作用。
- 每次调用返回一个可序列化结果；调用 ID 继续来自原始 `llm.ToolCall.ID`。第四天 Runner 再把它写入 `RoleTool` 消息，不在本日实现消息循环。
- 只为 Git 内容读取保留必要的可注入函数/小接口，生产实现调用 `ReadHeadFile`，单测可使用内存内容；不为每个结构增加接口。

**测试重点**：三个定义及 Schema 可解析、定义顺序稳定、未知工具、非法 JSON、尾随值、未知字段、缺失/null 必填字段、类型错误、取消识别。取消测试不依赖网络。

**完成标准**：合法调用到达正确工具；不合法调用产生统一错误，collector 保持不变。

## 步骤 3：实现 file_read 的固定版本与有界返回（60 分钟）

**准备编写**：`internal/tool/file_read.go`、`file_read_test.go`；必要时小幅调整 gitdiff 的共享校验入口。

**实施内容**

1. 校验仓库相对路径，使用 `/`，拒绝绝对路径、盘符、空路径、反斜杠、`..` 路径段和控制字符；允许普通空格路径。复用已有 Git 路径保护，不拼接 shell 或读取工作区。
2. 仅允许固定 head 中的普通 `.go` 文本。按路径段排除 vendor/testdata，排除标准生成标记、NUL 和非 UTF-8；符号链接、子模块、目录和超过 256 KiB 的文件由读取层拒绝。
3. 关联文件没有 patch，不能为它调用 Load 或用“没有 AddedLines”拒绝读取。路径/内容静态校验尽量共享，避免与第二天规则漂移；不把读取范围限制混入 gitdiff.Load。
4. 定义统一行切分：空文件为 0 行，末尾换行不额外产生一行；CRLF 去掉行尾 CR，保留正文中的空格和内部字符。当前文件评论计数与读取使用同一规则。
5. 返回 path、固定 head SHA、带行号 content、total_lines、实际 start_line/end_line 和 truncated；必要时包含 last_line_truncated，清楚说明末行正文被截断。

**范围默认值与校验顺序**

| 输入 | 计划行为 |
| --- | --- |
| 不传两端 | 从 1 开始，最多前 120 行，默认窗口在 EOF 处收缩 |
| 只传 start_line | 从该行开始，默认最多 120 行，窗口在 EOF 处收缩 |
| 只传 end_line | start_line 默认为 1，显式 end_line 必须有效 |
| 两端都传 | 要求 `1 <= start <= end <= total_lines` |
| 显式 0、负数、颠倒或超过 EOF | 错误，不静默夹紧显式非法值 |
| 合法范围超过 200 行或返回字节上限 | 截断成功，返回实际覆盖范围及标记 |
| 空文件且不传行范围 | 成功返回空 content、total_lines=0，两端为 null |
| 空文件且显式请求行号 | 范围错误 |

每次最多返回 200 行；带行号 content 的 UTF-8 字节数最多 16 KiB，元数据和 JSON 转义不计入此正文预算，在文档中明确口径。优先在完整行边界截断；单行本身超限时，仅返回能放入预算的 UTF-8 完整字符前缀，设置 `truncated=true`、`last_line_truncated=true`，不得假装返回完整行。避免整数溢出：先校验 start，再按剩余行数计算默认窗口。

**测试重点**

- 默认 120 行、单端参数、200/201 行、16 KiB 边界及超限，长单行和多字节字符不被拆坏。
- 空文件、末尾有/无换行、CRLF、范围 0/负数/颠倒/越界。
- 未变更关联文件、空格路径、缺失文件、排除目录、生成文件、二进制、非 UTF-8、特殊文件及 256 KiB 边界。
- 已取消 context；分支移动和工作区修改后，读取仍来自同一个 Snapshot.HeadSHA。

**完成标准**：内容有真实 head 行号、上限及截断信息；普通关联文件可读，排除文件不可读，工作区和索引不被改变。

## 步骤 4：实现 code_comment 校验与 collector（55 分钟）

**准备编写**：`internal/tool/code_comment.go`、`collector.go` 及对应测试。

**实施内容**

- 参数复用 `model.Finding` 的全部字段，要求必填。文本 TrimSpace 后不能为空；长度按 Unicode 字符数（Go rune）计数，title 最多 120 字符，其余文本字段各最多 2,000 字符。
- severity 只接受 high/medium/low，category 只接受 bug/security/performance；不自动把非法枚举转换成默认值。
- Path 必须精确等于当前任务文件，不做自动路径纠正；关联文件即使 file_read 成功，也不能成为评论目标。
- 验证 `1 <= StartLine <= EndLine <= 当前 head 总行数`，并要求 StartLine 属于当前 AddedLines。EndLine 可以覆盖后续上下文，但不得超过 EOF；不要求范围内每一行都是新增行。
- 所有字段校验成功后才写 collector；任一错误不增加或改写结果。
- description 的“触发条件和影响”、suggestion 的“修复思路”写入英文说明及 Schema description；程序只能可靠校验格式和长度，不假定已证明问题语义真实。
- collector 使用普通 slice 保存顺序、map 保存去重键，串行使用，不增加锁、channel 或数据库。
- 去重键是结构化的 `path + start_line + end_line + 归一化 title`，避免直接拼接产生歧义。title 归一化约定为 TrimSpace、内部连续空白折叠为一个空格、转小写；不做语义或标点归一化。
- 重复提交返回成功：`accepted=true, duplicate=true`，保持首条内容不变；新提交返回 `accepted=true, duplicate=false`。读取 collector 返回副本，空结果为非 nil 空 slice。

**测试重点**：合法问题、所有非法枚举、空白/缺失文本、120/121 和 2,000/2,001 字符（含中文）、非当前文件、越界、非新增起点、允许覆盖上下文、标题归一化重复、不同位置/标题不合并、失败无副作用、返回值不能改变内部状态。

**完成标准**：collector 只保存格式与锚点合法的问题，精确重复不增加结果；合法性校验不冒充问题真实性判断。

## 步骤 5：实现 task_done 与第四天交接契约（25 分钟）

**准备编写**：`internal/tool/task_done.go`、`task_done_test.go`，完善设计记录。

**实施内容**

- summary 必须存在且为去除首尾空白后的非空字符串；使用步骤 2 的严格解码。
- 成功返回当前文件的完成声明与 summary；允许 collector 为空，不把 summary 隐式转成 Finding。
- 不直接退出进程、不将整个运行标记 completed、不写最终报告，也不吞掉调用方 context 取消。
- 在设计记录中明确：第四天 Runner 必须执行并回填同轮所有调用，随后检查“合法完成声明且同轮无工具错误”；仅普通文本、零条问题或单次工具成功都不能代表完成。
- 对绑定不同文件的工具执行器分别测试，确保声明与评论目标互不串用；collector 可在本次运行中共享，但完成信号属于单文件。

**完成标准**：合法 summary 得到声明，空/缺失/null summary 失败；无论成功失败，collector 内容都不变。当天不实现同轮状态机。

## 步骤 6：离线集成演示、回归与记录（45 分钟）

**准备完善**：`internal/tool/integration_test.go`、README、`notes/day3-design.md`。

**测试与演示**

1. 用 `t.TempDir()` 建两个提交的临时仓库，隔离全局/系统 Git 配置、空模板、局部身份、关闭 autocrlf 和 hooks。不同包的测试辅助函数不可直接调用 gitdiff 的未导出 test helper；在 tool 测试内保留最小 fixture，不为复用测试新建生产仓库管理器。
2. 调用真实 `Resolve → Load`，为一个 Reviewable 文件绑定执行器，同时保留一个未变更的关联 Go 文件。
3. 直接通过注册表调用 `file_read → code_comment → 重复 code_comment → task_done`，不使用模型或 Mock Client。断言固定 head、实际行号、AddedLines 锚点、collector 长度为 1、重复标记和完成声明。
4. 插入未知工具、坏 JSON、读失败、非当前文件和非新增行评论，验证失败结果及 collector 无副作用；这些是工具调用测试，不宣称已运行可恢复的 Runner。
5. 提交后改变暂存区与工作区，验证读取仍为固定 head，工具调用前后的 status 和 cached diff 一致。链接 fixture 用 Git 索引对象构造，不依赖 Windows 链接权限。
6. 增加 `TestToolsOfflineDemo`，用 `t.Logf` 输出工具名、固定 SHA、成功/错误结果、duplicate、collector 数量及完成声明；所有输出用英文，不写报告文件。
7. 格式化、全部测试与构建，重新检查第二天 diff 演示和第一天 CLI 行为。命令仅在实施完成后运行，本文不把计划验证写成已通过。

```powershell
Set-Location E:\study\Mini-code-review
$env:GOTOOLCHAIN = 'local'
$env:GOPROXY = 'off'
$env:GOSUMDB = 'off'
gofmt -w .\internal\tool
# 若调整了共享 gitdiff 校验入口，也格式化该包
go test ./...
go build ./...
go test ./internal/tool -run '^TestToolsOfflineDemo$' -v
go test ./internal/gitdiff -run '^TestLoadDisplaysNumberedDiff$' -v
git diff --check
```

**文档记录**

- README 区分“工具可在测试中独立执行”与“CLI/Agent 已完成工具闭环”；后者继续标为未实现。
- day3-design 记录参数/返回契约、行数及字节口径、截断样例、锚点例子、去重规则、取消传播和实际命令结果。
- 新 Go 源码加 SPDX 头并保持 LF；新标识符、工具说明、诊断和测试输出使用英文，已有中文学习注释保留。
- 继续使用本项目 Go 工具链，不调用参考仓库 Makefile、不修改参考项目、不发起网络请求、不配置 API Key。

**完成标准**：独立工具演示、全部测试和构建通过；已有 CLI help/退出码不变；验证期间只对临时 fixture 仓库执行 Git 写操作。

## 当天交付与验收

建议新增 `internal/tool/`，按实际代码规模拆分 registry、file_read、code_comment、collector、task_done 和测试文件；另写 `notes/day3-design.md` 并更新 README。复用已有 Git、模型和客户端契约，避免重复建设。

- [ ] 三个工具有稳定名称、英文说明、JSON Schema 和程序端严格校验。
- [ ] file_read 从固定 head 读取关联 Go 文件，路径/类型/内容排除一致。
- [ ] 默认 120 行、最多 200 行/16 KiB、256 KiB 文件上限及截断契约经过验证。
- [ ] 评论只允许当前文件，起点属于 AddedLines，行号与 head 内容一致。
- [ ] 文本长度、枚举、精确去重和失败无副作用均有测试。
- [ ] task_done 只提供单文件完成声明，允许零问题，不承担 Runner 判断。
- [ ] 离线直接调用工具的演示通过，第一天和第二天测试未回归。
- [ ] README 和设计笔记记录实际成果，第四天可接入 Mock 和 Runner。

如果时间不足，优先完成固定版本读取、锚点校验、collector 和严格调用契约，减少额外接口与展示美化。未完成的边界在记录中明确列出，不勾选完成；不提前扩展 code_search、并发、语义去重或模型调用。

复盘时应能独立解释：为什么 Schema 不能代替程序校验；为什么关联文件可读但不可评论；为什么 start_line 必须是新增行而 end_line 可以覆盖上下文；为什么重复评论算成功；为什么 task_done 工具成功仍不代表 Runner 可以立刻结束。
