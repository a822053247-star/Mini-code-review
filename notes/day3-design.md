# 第三天设计记录：工具契约初稿

日期：2026-10-10。对应 [plan3.md](../plan3.md) 步骤 1；依据 [需求文档](../mini-code-review-agent-requirements.md) 第 7 节和现有第二天接口。

**状态：步骤 1 的契约阅读、步骤 2 的固定注册表/参数解码以及步骤 3 的 file_read 已实现；code_comment/collector、task_done 能力以及 Runner 尚未实现。本文 JSON 示例仍为协议样例，不是真实问题结论。**

## 阅读结果与复用范围

| 已阅读文件 | 观察及 Mini 约定 |
| --- | --- |
| 本项目 `internal/gitdiff/git.go` | Resolve 固定完整 SHA；ReadHeadFile 从 head 树检查模式/大小后读取 blob，已有路径保护、256 KiB 上限和 context 传播 |
| 本项目 `internal/gitdiff/load.go`、`types.go` | Load 保存筛选原因、完整 head 内容和 AddedLines；只为 Reviewable 文件创建评论任务 |
| 本项目 `internal/model/review.go` | 复用 Finding、Severity、Category；Report 只保存结果，不承担参数校验 |
| 本项目 `internal/llm/client.go` | 复用 ToolDefinition 和 ToolCall；Arguments 是 RawMessage，tool 消息用 ToolCallID 关联 |
| 参考 `internal/tool/definitions.go` | Provider 管单项能力，Registry 按名称查找；Mini 用固定三个名称的 map，不引入 Dynamic、Freeze 或额外工具 |
| 参考 `internal/tool/file_read.go` | 展示真实行号、实际范围和截断；参考参数为 file_path、最多 500 行，Mini 参数为 path、默认 120/最多 200 行及 16 KiB 正文预算 |
| 参考 `internal/tool/filereader.go` | 读取来源与展示分开，支持工作区/范围/提交；Mini 只支持固定 Snapshot.HeadSHA，复用已有 ReadHeadFile |
| 参考 `internal/tool/code_comment.go` | 参数解析和收集分开，但支持批量 comments、路径归一化及枚举默认值；Mini 一次接收完整 Finding，严格拒绝非法值和非当前文件 |
| 参考 `internal/tool/comment_collector.go` | 每次 Agent 有独立内存收集器，返回结果副本；Mini 每次运行一个 collector，slice/map 精确去重，串行使用无需锁或批次替换 |

参考 filereader 的 scanLines 在文件末尾有换行时额外计一行；Mini 不沿用这一口径。Mini 空文件为 0 行，`"a\n"` 为 1 行，`"a\n\n"` 为 2 行，末尾换行不制造额外锚点。CRLF 只去掉行尾 CR，保留正文空格及内部字符。读取和评论的总行数使用同一规则。

现有 gitdiff 的 validateFilePath、staticPathReason、generatedMarker 是包内标识符，tool 不能直接调用。步骤 2～3 需确定一个小型共享校验入口或等价集中实现，避免规则漂移。ReadHeadFile 已检查普通文件和大小，但不检查扩展名、目录、生成标记、NUL 或 UTF-8；这些检查不能因复用读取入口而遗漏。

## 单文件任务绑定与所有权

工具执行器在程序端绑定以下输入，模型无权覆盖：

- 本次运行已解析的 Snapshot，包括 RepoRoot、BaseSHA、HeadSHA。
- 一个来自该 Snapshot 的 Reviewable FileChange：固定 Path、AddedLines、HeadContent 和据此计算的总行数。
- 本次运行的 collector；不同运行不可共享，逐文件串行任务可共享同一实例。
- 必要的 Git 内容读取函数，生产使用 ReadHeadFile，单测可替换为内存读取。

构造阶段拒绝不可审查的任务、空 collector 或不满足绑定前提的输入；这些是程序装配错误，不交给模型修正。Snapshot 与 FileChange 的一致性由 Load 的调用链保证，FileChange 自身没有 SHA 字段，不能仅凭其字段验证提交来源。

Snapshot 是值类型；AddedLines 是 map、HeadContent 是 slice，浅复制不能隔离它们。绑定时复制锚点和必要内容，或从内容生成独立行数据，防止外部修改扩大评论范围。工具执行不更新任务 Path、锚点或 Snapshot，也不重新解析 HEAD。

file_read 的参数 path 可以指向未变更的关联 Go 文件；code_comment 的 path 必须精确等于当前任务 Path。读取成功不授权在关联文件上提交问题。

## 三个工具定义与参数 Schema

通过现有 llm.ToolDefinition 提供 Name、Description、Parameters，顺序固定为 file_read、code_comment、task_done。以下 Schema 是 Mini 的工具参数协议，不要求兼容参考项目或某个供应商的 HTTP 格式。

### file_read

英文说明：`Read numbered Go source from the fixed head commit, including unchanged related files. Reading a file does not authorize comments on it. Output is bounded and may be truncated.`

```json
{
  "type": "object",
  "properties": {
    "path": {"type": "string", "minLength": 1, "description": "Repository-relative Go file path using forward slashes."},
    "start_line": {"type": "integer", "minimum": 1, "description": "Optional first head line, inclusive. Defaults to 1."},
    "end_line": {"type": "integer", "minimum": 1, "description": "Optional last head line, inclusive. When omitted, read up to 120 lines from the start."}
  },
  "required": ["path"],
  "additionalProperties": false
}
```

仅允许 head 中普通 UTF-8 `.go` 文本，拒绝 vendor/testdata 路径段、标准生成标记、二进制、特殊文件以及非法路径。路径保持 Git 的字面形式，允许普通空格，不把 `../`、反斜杠或绝对路径清洗成合法输入。

默认窗口在 EOF 收缩；显式提供的行号必须在真实范围内，不能静默收缩非法 end_line。只传 end_line 时 start=1；只传 start_line 时默认最多 120 行。先验证完整请求范围，再执行最多 200 行/16 KiB 截断，避免用截断掩盖越界。

content 格式固定为 `N|正文\n`。16 KiB 统计这个字符串的 UTF-8 字节数，包括行号、分隔符和展示换行；不含元数据及 JSON 转义后的体积。完整文件仍最多 256 KiB。第四天的整体消息预算需独立检查序列化后的实际消息体。

truncated 表示合法的有效请求窗口未完整返回，不表示窗口外还有文件内容：300 行文件默认返回前 120 行且未碰到字节限制时为 false。实际 start_line/end_line 表示已返回的覆盖行，而不是原始请求边界。

优先按完整行截断。若第一条待返回行本身超过正文预算，返回能容纳的 UTF-8 字符前缀，保留行号，并设置 last_line_truncated=true；不得拆开 UTF-8 字符或暗示正文完整。若已返回完整行后下一行放不下，则在完整行边界停止。空文件默认读取成功，content 为空、total_lines=0、两端为 null；显式请求任何行号失败。

### code_comment

英文说明：`Submit one finding for the current review file. The start line must be an added head line. Describe the trigger and impact, and provide a repair approach. Valid duplicate submissions succeed without adding another finding.`

```json
{
  "type": "object",
  "properties": {
    "path": {"type": "string", "minLength": 1, "description": "Must exactly match the current review file."},
    "start_line": {"type": "integer", "minimum": 1, "description": "Head line belonging to the current file's AddedLines."},
    "end_line": {"type": "integer", "minimum": 1, "description": "Inclusive head end line; must not precede start_line or exceed EOF."},
    "severity": {"type": "string", "enum": ["high", "medium", "low"]},
    "category": {"type": "string", "enum": ["bug", "security", "performance"]},
    "title": {"type": "string", "minLength": 1, "maxLength": 120, "description": "Short problem title."},
    "description": {"type": "string", "minLength": 1, "maxLength": 2000, "description": "Explain the triggering conditions and their impact."},
    "suggestion": {"type": "string", "minLength": 1, "maxLength": 2000, "description": "Describe a repair approach; a full patch is not required."}
  },
  "required": ["path", "start_line", "end_line", "severity", "category", "title", "description", "suggestion"],
  "additionalProperties": false
}
```

所有必填字段拒绝缺失或 null，所有文本字段拒绝纯空白。title/description/suggestion 去除首尾空白后保存，按 Unicode rune 数检查长度；path 与枚举不做自动修正。要求 `1 <= start <= end <= total_lines` 且 start 属于绑定的 AddedLines；end 可覆盖后续上下文，不必逐行都在新增集合中。

校验全部成功后再写 collector。去重键使用结构化的 path、start、end、归一化 title；title 归一化为去首尾空白、内部连续空白折叠成一个空格、转小写。重复返回成功，保留首次保存的完整 Finding，不覆盖 description/suggestion。collector 以首次接受顺序返回副本，空结果为 `[]`。

程序能校验格式、范围和锚点，不能证明 description 中的触发条件/影响真实，也不做语义去重、代码修复或问题重新定位。

### task_done

英文说明：`Declare that the current file has been checked and summarize the checks. This may be used with zero findings. The runner decides whether the declaration completes the file task.`

```json
{
  "type": "object",
  "properties": {
    "summary": {"type": "string", "minLength": 1, "description": "Non-blank summary of what was checked in the current file."}
  },
  "required": ["summary"],
  "additionalProperties": false
}
```

summary 去首尾空白后必须非空；不接受缺失/null。工具不保存 Finding，不修改 Report，不设置全局完成状态。零条问题不妨碍声明完成。

以上 Schema 只描述静态约束；跨字段范围、当前任务身份、动态 EOF/AddedLines、纯空白等必须由程序校验。可选行号缺省与显式 0/null 要区分，显式 null 失败；拒绝非对象、未知字段、类型错误、尾随第二个 JSON 值及坏 JSON，不修复模型参数。

## 统一返回与取消传播

步骤 2 已采用 `Registry.Execute(ctx, llm.ToolCall) → (Execution, error)`；Execution 嵌入 Result（JSON envelope），另带不参与 JSON 序列化的可选 Completion。以下行为固定：

| 场景 | execution | Go error | 副作用 |
| --- | --- | --- | --- |
| 读取成功、评论成功（含重复）、合法 task_done | 成功 JSON，task_done 另带完成声明 | nil | 只有首次合法评论写 collector |
| 未知工具、坏参数、路径/范围错误、读取失败 | 失败 JSON，无完成声明 | nil | 无新增评论 |
| context 取消或 deadline 超时，包括 Git 子调用超时 | 零结果，无完成声明 | 保留原始 cause，可 errors.Is 识别 | 不接受新的评论/完成声明 |

普通失败通过 JSON 回填，让未来模型修正；取消/超时走 Go error 控制流，不能只转成一句字符串。包装必须用 `%w` 或保留 Unwrap，不用错误文本匹配来识别取消。调用开始检查 ctx.Err，读取后及评论写入/完成返回前再次检查；此前已接受的问题仍保留。串行模式不承诺对异步取消与单次内存写入提供事务回滚。

所有工具普通失败使用英文 code/message。初始错误码约定如下，具体校验顺序先解码、再字段约束、再任务/文件约束：

| code | 对应情况 |
| --- | --- |
| `unknown_tool` | 名称未注册 |
| `invalid_arguments` | JSON、缺失/null、类型、枚举或文本约束错误 |
| `invalid_path` | 非法路径 |
| `file_excluded` | 扩展名、目录、生成文件、二进制或 UTF-8 排除 |
| `special_file` | ReadHeadFile 返回 ErrNotRegularFile |
| `file_too_large` | ReadHeadFile 返回 ErrFileTooLarge |
| `read_failed` | 其他非取消的 Git 内容读取失败 |
| `invalid_range` | 读取或评论行号范围非法 |
| `wrong_file` | 评论目标不是当前任务文件 |
| `invalid_anchor` | 评论起点不属于 AddedLines |
| `tool_failed` | 处理函数返回意外非取消 Go error，或产生矛盾/不可序列化的结果 |

现有 ReadHeadFile 对缺失路径没有专用 sentinel，先统一用 read_failed，不能从错误字符串猜测 file_not_found。取消优先用 errors.Is 检查，不落入普通 read_failed。绑定/装配错误由构造阶段返回 Go error。

成功只包含 ok/data，失败只包含 ok/error，不输出两种相互矛盾的分支。样例：

```json
{
  "ok": true,
  "data": {
    "path": "sample.go",
    "head_sha": "2222222222222222222222222222222222222222",
    "content": "4|\t// Add both inputs.\n5|\treturn a + b\n",
    "total_lines": 6,
    "start_line": 4,
    "end_line": 5,
    "truncated": false,
    "last_line_truncated": false
  }
}
```

SHA 是占位示例；实际返回绑定的完整 head SHA。评论及重复评论分别返回：

```json
{"ok": true, "data": {"accepted": true, "duplicate": false}}
```

```json
{"ok": true, "data": {"accepted": true, "duplicate": true}}
```

非新增起点示例：

```json
{"ok": false, "error": {"code": "invalid_anchor", "message": "start_line must be an added line in the current file"}}
```

完成声明示例：

```json
{"ok": true, "data": {"done_requested": true, "path": "sample.go", "summary": "Checked arithmetic and boundary cases."}}
```

execution 另携带程序可见的可选 Completion（当前 path、summary），只由合法 task_done 产生，不接受模型提交的 done_requested/path 字段，也不靠搜索结果字符串来判断完成。JSON 的 done_requested 表示声明已收到，不表示任务已经完成；两种表示必须由同一次合法结果生成并保持一致。

工具结果正文不自行创建 messages 或重复嵌入 tool_call_id。第四天 Runner 使用原 ToolCall.ID 创建 RoleTool 消息，Content 为结果 JSON；取消后若需要记录该调用的失败消息，由 Runner 构造 cancelled/deadline_exceeded 结果，同时继续保留可识别的 Go cause 并停止相应执行，不能静默当普通可恢复参数错误。

## 工具成功、完成声明和最终完成

| 概念 | 判定者 | 含义 |
| --- | --- | --- |
| 工具成功 | 工具/注册表 | 当前读取、评论或声明符合契约；不表示代码无问题或文件已完成 |
| 当前文件完成声明 | task_done | 收到合法 summary，可在零问题时产生；不决定 Runner 停止 |
| 当前文件任务完成 | 第四天 Runner | 同轮全部调用已执行和回填，收到合法完成声明，且同轮无工具错误 |
| 最终运行完成 | 后续 Agent 汇总 | 所有应审查文件完成且无覆盖缺口；根据需求汇总 completed/partial/failed 等状态 |

例如，同轮先合法 task_done 再非法 code_comment：声明工具成功，但整轮有错误，Runner 不能接受完成。反过来，同轮合法 task_done 后还有合法评论：必须执行并保存评论，再判定文件完成。以上状态机本日只记录，留到第四天实现。

## 后续测试的可观察契约

- 固定读取：关联文件读取不会改变当前评论 Path/AddedLines；工作区或分支改变后返回同一 head 内容。
- 返回边界：默认/单端范围、空文件、120/200 行窗口、16 KiB 正文、UTF-8 长行、末尾换行及 CRLF 与本文一致。
- 参数失败：未知工具、坏 JSON、未知/缺失/null 字段、尾随值和非法枚举产生失败 JSON，无完成声明、无 collector 增长。
- 评论：起点属于新增集合即可，终点可含上下文；重复成功但只保留首条；返回副本不能改写 collector。
- 完成：空 collector 也可收到声明；summary 不变成 Finding；失败声明无副作用。
- 取消：errors.Is(err, context.Canceled/DeadlineExceeded) 为 true，不能仅通过 JSON message 验证；取消前已接受的 Finding 不丢失。
- Schema 与说明：三个定义以固定顺序提供，参数对象可解析，英文说明与实际校验一致。

## 本次步骤 1 的成果与未完成事项

已阅读需求、本项目五组接口以及指定的五个参考文件，并确定任务绑定、三个名称/说明/Schema、返回 envelope、完成声明、取消传播和精确去重规则。参考项目仅作只读阅读，没有修改或直接导入其 internal 包。

步骤 1 仅新增本文，没有创建 Go 源码、执行工具能力或运行测试。其后的步骤 2 进度单独记录如下，不把完整工具能力标记成已完成。

## 步骤 2：固定注册表与严格参数解码

新增 `internal/tool/registry.go` 和 `registry_test.go`，没有实现模型、Runner 或三个工具的业务能力。

- `NewRegistry(Handlers)` 要求三个具名处理函数齐全，按固定名称 map 分派；不存在动态注册或插件接口。FileReadArgs 用 `*int` 保留缺省，CodeCommentArgs 复用 model.Finding，TaskDoneArgs 明确定义 summary。
- `Definitions()` 以 file_read/code_comment/task_done 顺序返回 llm.ToolDefinition；每次返回独立 Schema 字节，调用方修改不会影响下次结果。
- 解码先用标准库 token 检查对象、精确字段名、重复字段、null、必填字段和单一 JSON 值，再解码具体字段类型。额外拒绝大小写变体和重复键（包括转义后重名），避免 encoding/json 的宽松匹配和合并行为。
- 静态字段约束在分派前检查：正整数、非空白字符串、枚举、Unicode 字符长度；title/description/suggestion 和 summary 按已定规则去首尾空白。路径合法性、EOF、AddedLines、实际读取和去重仍留给具体工具。
- `Success`/`Failure` 构造 envelope；执行结果检查分支一致性与可序列化性。普通处理失败清除 Completion，非 task_done 结果也不能产生 Completion。调用 ID 和参数保持原样，不创建 RoleTool 消息。
- 调用前/分派前/处理函数返回后检查 context；子调用返回的 wrapped Canceled/DeadlineExceeded 保持 errors.Is 可识别。普通未知工具/解码失败为 JSON 失败；意外处理函数错误统一为 tool_failed，不泄露原始后端诊断。

能力通过三个带类型的具体函数闭包绑定，无额外 Provider 接口。注册表自身不读取 Git；步骤 3 的 file_read 处理函数将绑定 Snapshot 和 `gitdiff.ReadHeadFile`，单测可在该闭包内注入同签名的内存读取函数。当前分派单测使用 handler spy，不假装执行了生产读取或 collector；不合法调用不会到达 spy，已有模拟问题集合保持不变。

测试覆盖定义/Schema 和返回顺序、调用方修改隔离、缺失 handler、合法具名分派、可选行号缺省、未知工具、坏 JSON、非对象、尾随值、未知/重复字段、所有字段缺失/null/类型错误、静态边界、无副作用、成功/失败序列化、子调用取消与结束信号隔离。取消测试完全使用内存回调，不请求网络或真实模型。

2026-10-10 实际验证：`gofmt -w ./internal/tool` 完成；`go test ./internal/tool`、随后 `go test ./...` 全部 6 包通过，`go build ./...` 通过，`git diff --check` 通过。新 Go 文件均已检查 SPDX 头和 LF。Go 命令设置 GOTOOLCHAIN=local、GOPROXY=off、GOSUMDB=off；没有网络请求，第一天 CLI 和第二天 Git 测试继续通过。没有修改参考代码、用户仓库索引或被审查工作区。

下一步为 plan3 步骤 3：绑定固定 Snapshot 的 file_read，实现路径/文本排除、真实行范围及有界输出；随后再实现 collector、动态评论锚点校验和 task_done 能力。

## 步骤 3：固定 head 的 file_read 与有界输出

2026-10-11 新增 `internal/tool/file_read.go`、`file_read_test.go`。

- `NewFileReader(snapshot, read)` 验证 Snapshot 格式并保存值副本。read 为 nil 时使用生产 `gitdiff.ReadHeadFile`，测试可注入同签名的内存读取函数；`reader.Execute` 可直接赋给 `Handlers.FileRead`，无需变更注册表或已有 CLI。
- gitdiff 导出 `SourcePathReason`、`SourceContentReason`，Load 和工具共用原有路径、扩展名、目录、生成标记、NUL、UTF-8 筛选。另共享 `ValidateSnapshot` 和 `MaxHeadFileBytes`；没有把读取窗口规则放入 Load。
- 不要求关联文件有 patch 或 AddedLines，也不再调用 Load 判断读取资格。普通空格路径及 myvendor/mytestdata 可读，vendor/testdata 路径段被排除。
- `SplitSourceLines` 实现已约定的空文件、末尾换行与 CRLF 口径，供步骤 4 的评论总行数计算复用。
- 成功 data 为 `FileReadData`，包含 path、head_sha、带行号 content、总行数、实际两端和截断标记。默认窗口为 120 行，合法显式请求最多返回 200 行；非法显式端点先报错，不用截断掩盖越界。
- 正文预算包含 `N|` 和展示换行，最多 16 KiB UTF-8 字节；不含 JSON 转义及元数据。第一行太长时按完整字符截断并标记 last_line_truncated；已有完整行时在下一行前停止。空文件默认两端为 null；显式请求空文件行号失败。
- Git 读取错误按约定映射为 special_file/file_too_large/read_failed；context 取消与子请求超时仍通过 wrapped Go error 传播。读取后及输出过程中检查取消，普通成功不产生 Completion。

测试以小型内存 fixture 验证 120 默认窗口、单端参数、EOF、200/201 行、非法/极大整数端点、16 KiB 恰好/超一字节、UTF-8 长单行、完整行边界、空文件及 CRLF。真实 Git fixture 用局部身份、空模板、关闭 autocrlf/hooks 的临时仓库，验证未变更关联文件、普通空格路径、256 KiB 边界/超限、目录、生成/二进制/非 UTF-8、索引构造链接与子模块。

集成测试经真实注册表执行 file_read。固定 Snapshot 后创建第三次提交移动分支，再留下不同暂存/工作区内容并删除关联文件；返回 main.go 的旧 head 内容及关联文件提交内容。读取前后的 status 与 cached diff 相同。其余两个处理函数仍为测试 spy，不宣称已经完成三个工具闭环。
