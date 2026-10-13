# 第四天设计记录：Runner 契约与脚本 Mock

日期：2026-10-12。对应 [plan4.md](../plan4.md) 步骤 1～3，依据 [需求文档](../mini-code-review-agent-requirements.md) 第 6、8、10～11 节。

**状态：脚本 Mock、初始上下文、单文件 Runner、停止条件与最小串行调度已实现，并可通过离线单测执行。真实 Git + Runner 正式演示、最终报告状态汇总及 CLI 接入仍未实现。**

## 已阅读与复用的契约

| 现有接口 | 使用约定 |
| --- | --- |
| `llm.Client.Complete(ctx, Request) (Response, error)` | 一次模型请求；Mock 与未来真实客户端实现相同接口，Runner 不判断客户端类型 |
| `Request.Messages/Tools` | 初始消息与后续完整历史，以及真实 Registry.Definitions 提供的定义 |
| `Message.ToolCalls/ToolCallID` | 先保留 assistant 原始调用，再逐个追加 RoleTool 结果；包括失败调用 |
| `Response.Usage` / `TokenUsage` | nil 表示未知，指向零表示已知零，不能相互转换 |
| `Registry.Execute` / `Execution.Result` | 普通工具失败返回结构化 JSON，取消/超时返回可 errors.Is 识别的 Go error |
| `Execution.Completion` | 仅合法 task_done 产生，属于当前文件，不参与 JSON 序列化；工具成功不代表 Runner 接受完成 |
| `model.FileResult/Usage` | 保存单文件状态、停止原因、实际请求次数与用量，不创建另一套结果字段 |
| `tool.Collector` | 由装配方创建并持有；Runner 停止不能清空已接受评论 |

现有接口足以确定本步契约，没有读取或修改参考项目，也没有新增框架依赖。

## Runner 输入输出契约

`internal/agent/runner.go` 已按步骤 1 约定实现以下接口：

```go
type RunnerOptions struct {
    MaxRounds       int
    MaxContextBytes int
    RequestTimeout  time.Duration
}

func NewRunner(client llm.Client, registry *tool.Registry, options RunnerOptions) (*Runner, error)
func (runner *Runner) Run(ctx context.Context, path string, initial []llm.Message) (model.FileResult, model.Usage, error)
```

装配方先用同一 Snapshot 的 Reviewable FileChange 创建 reader/commenter/finisher，并绑定真实 Registry。path 与 registry 的任务来源一致性由装配方保证，Runner 不根据模型参数重新选择任务。Runner 不持有或清空 collector，不加载 Git，不输出报告。

构造阶段检查 client/registry 非 nil；options 零值使用 8 轮、64 KiB、60 秒默认值。MaxRounds 显式值限 1～20；上下文与超时显式值必须为正且不超过 64 KiB/60 秒，可在测试中设较小限制，生产装配沿用需求默认值。负数为装配错误，不当成默认值。初始消息保存独立副本，每文件重新开始；工具定义来自 registry，不从 Mock 提供的文本获取。

Run 返回 FileResult（Path、Status、StopReason、Rounds）和本文件 Usage。完成为 completed/task_done；普通停止为 failed，并记录 invalid_response/max_rounds/context_limit。请求或工具不可继续的 Go error 同时返回失败结果与 wrapped cause，不丢失已有统计。父 context 取消优先检查 ctx.Err，通过 errors.Is 保留 cause；调度方据此停止全局。局部模型错误/请求超时则记录文件失败并继续下一文件。Runner 不调用 os.Exit。

Rounds 与 LLMRequests 统计实际发起的请求，包括失败请求；ToolCalls 统计实际开始分派的调用，包括普通失败、重复评论及 task_done。未执行的后续调用不虚增数量。token 输入/输出分别累加，任一次请求对应项未知则该项总量为 nil；零请求为已知零。全局汇总留给后续调度/报告层。

同轮完成规则已在步骤 2 实施：先保存 assistant，串行执行全部普通调用，逐个回填 Result，最后检查合法 Completion 且同轮无错误。完成不能跳过后续评论，也不能沿用上一轮被拒绝的声明。

## 已实现的 Mock API 与行为

`internal/llm/mock.go` 提供：

```go
type MockStep struct {
    Response Response
    Error    error
}

func NewMockClient(steps []MockStep) *MockClient
func (client *MockClient) Complete(ctx context.Context, request Request) (Response, error)
func (client *MockClient) Requests() []Request
```

`MockClient` 编译期断言实现 Client，仅用于串行离线请求，不提供并发保证。NewMockClient 接收完整内存脚本并复制，不解析响应语义。空脚本允许构造，但第一次调用即耗尽。

| 情况 | 返回值 | 脚本与记录 |
| --- | --- | --- |
| 有 Response 的有效步骤 | 独立 Response，nil error | 记录 Request，消费一项 |
| 有 Error 的步骤 | 零 Response，原始 error | 记录 Request，消费一项，下一次继续下一项 |
| 同一项同时设置 Response 和 Error | Error 优先，零 Response | 同上，不泄露成功分支 |
| 脚本耗尽 | 零 Response，`ErrMockScriptExhausted` | 仍记录有效请求，不重复最后响应 |
| 调用开始 context 已取消或 deadline 已过 | 零 Response，wrapped cause | 不记录，不消费 |

耗尽错误文本为 `mock response script is exhausted`，可以通过 errors.Is 判断。Complete 在入口和复制之后检查 context；预设 error 原样返回，保留身份与 Unwrap 链。Mock 本身没有阻塞等待，也不模拟 60 秒网络超时；未来 Runner 超时测试使用等待 ctx.Done 的测试 Client。异步取消与最终内存写入之间不承诺事务回滚。

## 所有权与坏 JSON 参数

复制使用 slices.Clone 和逐字段指针复制，不使用 JSON marshal/unmarshal：

- 构造时复制步骤切片、Response.Assistant.ToolCalls、每条 Arguments、Usage 与输入/输出 token 指针。
- Complete 返回再次复制的 Response；共享同一个源响应的多个脚本步骤彼此隔离。
- 记录 Request 时复制 Messages、嵌套 ToolCalls/Arguments、Tools 及每个 Parameters；后续 Runner 追加历史或调用方修改请求不会改写记录。
- Requests 每次返回独立快照，空记录为非 nil 空 slice；嵌套 nil 与显式空切片保持原形，Usage 的未知/已知零也保持原形。
- 字符串为值，error 按不可变错误契约共享，不为任意自定义 error 对象构造通用深拷贝。

例如坏参数 `{"path":` 会按原始字节保留，Mock 不修复或拒绝。后续 Registry 负责产生 invalid_arguments，Runner 负责回填；通过 JSON 往返复制会提前失败，无法测试这条真实错误恢复路径。

## 本步测试范围

`mock_test.go` 的显式脚本依次返回 related.go 的 file_read、sample.go 第 5～6 行的完整 code_comment、task_done，使用稳定 ID。它只证明模型接口的脚本顺序，不执行工具或分析任意 Go 代码，不宣称 Runner 闭环完成。

测试还覆盖脚本耗尽及重复耗尽、空脚本、预设错误优先与消费、Canceled/DeadlineExceeded 可识别且不消费、构造输入/返回响应/记录请求/快照的嵌套修改隔离、坏 JSON 原样保留、nil Usage、部分未知 token、已知零与空集合。

## 实际验证

2026-10-12 实际验证使用本项目 Go 1.26.7 工具链，设置 GOTOOLCHAIN=local、GOPROXY=off、GOSUMDB=off，不进行 HTTP、网络请求或 Key 配置。

| 实际命令/检查 | 结果 |
| --- | --- |
| `gofmt -w ./internal/llm/mock.go ./internal/llm/mock_test.go` | 完成，后续 gofmt -l 无输出 |
| `go test ./internal/llm -run '^TestMockClient' -v` | 六个测试函数及取消子测试全部通过，包耗时 0.348 秒 |
| `go test ./...` | 全部 6 包通过，llm 0.231 秒，其余为缓存结果 |
| `go build ./...` | 通过 |
| `git diff --check` | 通过 |
| 两个新 Go 文件 SPDX/LF 检查 | 通过 |

步骤 1 未修改现有客户端/工具契约、CLI、参考代码或项目 Git 索引，没有创建报告/trace。当时前三天测试继续通过，Runner 完成判断与停止条件尚未实施，Mock 脚本单测并不代表完整 Agent 审查。

## 步骤 2：初始上下文、消息回填与同轮完成

新增 `internal/agent/context.go`、`runner.go` 和各自测试。没有更改 Git、工具或客户端协议。

`BuildInitialMessages(set, path, background)` 接收已加载 ChangeSet，从其中选择一个 Reviewable 且有 AddedLines 的当前文件；拒绝无效 Snapshot、缺失/跳过任务和重复当前路径。只使用内存数据，不运行 Resolve/Load 或访问 Git。返回独立的 system/user 两条消息，固定英文 system 说明审查边界、工具协议与同轮错误规则。

user 的 Content 是 JSON 数据对象：background、base_sha、head_sha、changed_paths、current_path、numbered_patch、added_lines。完整变更列表包括跳过文件并按路径排序；新增行集合排序。patch 来自已有 ParsedPatch.Lines，标注 `+ base:- head:5`、`- base:4 head:-`、上下文 `base:5 head:6`，不存在的一侧用 `-`，删除行不伪造 head 锚点。背景/注释/源码保留为 JSON 字符串数据；嵌入的指令不进入 system。这是数据边界与提示词约束，不是对模型抗注入能力的保证。

Run 要求允许的 Go 路径和恰好两条无调用/结果的 system/user 初始消息；不接收外部历史恢复。每次运行创建本地 history，每次请求再次复制历史及重新取得独立工具定义，防止客户端修改后污染下一轮。原始 assistant 参数逐字节保留，包括坏 JSON，不通过 JSON 往返复制。

响应必须为 assistant，不能带 ToolCallID，所有工具调用 ID 必须非空白且同轮唯一；整个响应校验通过后才执行第一个工具，避免前面的评论成功后才发现后面的 ID 无法关联。不合法响应不写入不完整的 assistant/tool 历史，追加英文纠正提示并计为无效轮次；合法纯文本则保留 assistant，再追加 task_done 提醒。

普通工具调用逐个执行并追加 RoleTool，Content 仅序列化 Execution.Result，ToolCallID 保持原值。普通未知工具、坏 JSON、缺失文件、非当前文件与非新增锚点均有失败 JSON，失败后继续本轮剩余调用。成功 task_done 还必须具有指向当前 path 的 Completion，否则回填 invalid_completion，阻止错误装配造成跨文件完成。

每轮独立维护 successful、roundFailed、done。只在全部普通调用处理后接受 done 且无失败；完成在前不漏后续评论，重复评论仍成功。出现错误的轮次，即使已有合法完成声明和合法评论，也不能完成；下一轮仅成功读取不会自动接受旧声明，必须再次 task_done。父取消或工具不可继续的 Go error 立即停止，不补造未执行调用的结果，collector 中此前合法问题保留。

为使本步循环有限，已经实现单文件保护：每次请求前检查轮次与消息正文/原始参数字节预算，60 秒派生请求 context，连续两轮无成功工具调用停止。成功读取/评论/重复/声明重置无效计数；最后允许轮次仍可完成；同轮已完成不额外检查下一次请求的预算。请求计数、工具计数和 token 累加复用 model.Usage，未知用量保持 nil，异常负数或累加溢出的 token 项也按未知处理。步骤 3 将补齐完整停止条件验收矩阵和逐文件调度；本步不提前宣称它们全部完成。

测试使用真实 Registry、FileReader、Commenter、TaskFinisher 和 collector；读取依赖采用显式内存 Go fixture，避免把单元测试说成真实 Git 集成。覆盖三轮消息顺序/原始参数/工具定义/调用 ID、固定 SHA 读取、零问题、重复、完成在前/在后、五类错误回填及后续恢复、无效角色/ID 无副作用、错误文件完成被拒绝、取消保留评论、模型失败/短超时、预算与轮次保护、客户端修改隔离及重复 Run 历史独立。未实现信号入口、调度、报告或 trace。

### 步骤 2 实际验证（2026-10-12）

所有 Go 命令设置 GOTOOLCHAIN=local、GOPROXY=off、GOSUMDB=off，使用本项目 Go 1.26.7，不调用网络或配置 API Key。

| 实际命令/检查 | 结果 |
| --- | --- |
| `gofmt -w ./internal/agent` | 完成，后续 gofmt -l 无输出 |
| `go test ./internal/agent -v` | 14 个测试函数及相关子测试通过，包耗时 0.622 秒 |
| `go test ./...` | 全部 7 包通过；agent 0.607 秒、llm 0.342 秒，其余为缓存结果 |
| `go build ./...` | 通过 |
| `git diff --check` | 通过 |
| 四个新 agent Go 文件 SPDX/LF 检查 | 通过 |

已更新 README 区分单文件 Runner 单测与后续真实 Git 集成、CLI/Agent 审查。没有修改参考项目、现有客户端/工具接口或 CLI，没有 Git 写操作、报告或 trace 输出。已有 CLI help/退出码测试继续通过。

步骤 2 完成时的下一步为 plan4 步骤 3；下面记录本次停止条件验收与最小逐文件串行调度，步骤 4 再增加真实 Git + Mock 的 TestRunnerOfflineDemo。

## 步骤 3：停止条件验收与最小串行调度

新增 `internal/agent/agent.go`、`agent_test.go`、`runner_limits_test.go`，完善 runner 的配置校验与实际用量记录。NewRunner/NewAgent 共享 options 默认值及上限校验，显式较大参数不能绕过 20 轮、64 KiB 或 60 秒保护；Git 仍沿用已有 10 秒子请求约束，没有修改 Git 层。

### 固定的停止顺序

| 检查位置 | 顺序与结果 |
| --- | --- |
| Run 入口 | 父 ctx.Err 优先为 canceled，再检查任务输入 |
| 每次模型请求前 | 父取消 → 实际请求数已达 max_rounds → 消息超出 context_limit → 请求模型 |
| 模型返回 | 立即释放该次派生 context；记录实际返回用量；父取消优先，否则请求 deadline 为 request_timeout，其余 Go error 为 model_error |
| 工具执行 | 父取消优先；普通 JSON 失败继续同轮，wrapped deadline 为 tool_timeout，其余不可继续 Go error 为 tool_error |
| 同轮处理结束 | 父取消 → 合法完成且无同轮错误 → 更新连续无成功轮数，达到两轮为 invalid_response → 下一轮检查 |

因此连续无效停止优先于轮次耗尽，同轮完成优先于下一次请求预算；已有成功调用但未完成时，下一轮的 max_rounds 优先于 context_limit。父取消覆盖当前局部错误，并用 wrapped cause 保留 errors.Is。纯文本、非法响应和全部调用失败使用同一个无效计数，合法读取、评论、重复评论或声明重置它。

预算精确累计所有 Message.Content 与历史 assistant ToolCalls.Arguments 的 UTF-8 字节，包括错误和成功工具 JSON。测试验证初始正文 65536/65537 字节、多字节字符，以及加上真实回填正文/原始参数后恰好 65536 或超过一字节；不计算 schema、角色、ID、路径元数据或 HTTP 转义体积。超限只阻止下一次请求，不截断历史；若同轮已经完成，不额外发请求。

请求取消函数在每次 Complete 返回后立即调用，测试检查前一轮 ctx 已释放、新一轮独立且默认 deadline 在 60 秒内，最终请求 ctx 也已释放。超时测试通过等待 ctx.Done 的 Client 注入短限制，不等待 60 秒。工具超时测试由既有 HeadFileReader 依赖返回 wrapped DeadlineExceeded，验证控制流和已接受评论保留；不伪称在单测中等待或运行了真实 10 秒 Git 超时。

### 最小调度 API 与结果所有权

```go
func NewAgent(client llm.Client, options RunnerOptions, read tool.HeadFileReader) (*Agent, error)
func (agent *Agent) Run(ctx context.Context, set gitdiff.ChangeSet, background string) (ReviewResult, error)
```

read 为 nil 时由既有 FileReader 读取固定 head Git 对象；测试可复用现有读取函数依赖。Run 接收已 Load 的 ChangeSet，不重新解析 ref、不重新加载或筛选文件、不引入生产仓库管理器。

每次运行复制 ChangeSet 的可变输入并按路径排序，为每个 Reviewable 文件建立新 FileReader/Commenter/TaskFinisher/Registry/Runner 与独立初始历史，共享该次运行新建的 collector。外部修改原 ChangeSet 不改变后续任务；重复 Agent.Run 不共享 collector、历史或统计。Snapshot 无权被模型覆盖。

ReviewResult 保存固定 Snapshot、所有文件的 FileResult、Findings 副本、Warnings 副本、HasCoverageGap、Usage 与 FileErrors；没有最终 RunStatus，不代替第六天 Report 状态算法。静态排除项保留原 SkipReason，零轮次、不请求模型；输入的 warnings 和覆盖缺口保留，任一任务未完成会增加覆盖缺口。零文件/全跳过仍返回非 nil 空集合，已知零请求与零 token；覆盖缺口不会因没有可执行任务而丢失。

局部模型错误/请求超时/工具超时记录到当前 FileResult 和 FileErrors，并继续下一文件；轮次、预算或无效响应的正常停止也继续，以状态与 StopReason 表示，没有伪造 Go error。完成整个调度返回 nil error 不代表所有文件成功，调用方必须检查 Files/HasCoverageGap/FileErrors。错误 cause 仅供程序消费，不直接序列化或写入报告。

父取消则立即停止，返回 wrapped cause；正在执行的文件 failed/canceled，后续未执行的 Reviewable 文件也为 failed/canceled、Rounds=0，静态 skipped 不改写。此前接受的 Finding 和请求统计保留。输入 Snapshot 格式或重复任务路径错误在模型调用前拒绝；单文件装配错误为 invalid_input，不冒充模型失败。

### 用量与测试结果口径

LLMRequests/Rounds 计实际发起的请求，包括失败；ToolCalls 计实际开始分派的调用，包括未知工具、参数失败、重复及结束声明，取消后未执行的调用不计。每次请求实际返回的 Usage 都参与统计，即使同时有 Go error；未提供 Usage 的失败请求使该项总量为未知。

输入/输出 token 分别累加，任一次该项未知则最终该项为 nil，已知零保持零；无请求为已知零。跨文件继续应用同一规则，失败不清空此前计数或评论。负数/溢出 token 仍按未知处理。单测覆盖部分字段未知、整个 Usage 缺失、溢出、零及跨文件累加。

本次覆盖默认 8 轮、1/20 边界、最后轮次完成、连续无效组合与成功重置、停止优先级、实际请求 context 生命周期、各类超时/失败保留问题、父取消/父 deadline、多文件排序、独立历史/注册表、局部失败及正常停止继续、取消不调度后续文件、空/排除输入、覆盖缺口保留和多次运行隔离。测试通过 Mock 与内存 Go fixture 执行真实工具，不宣称真实 Git 集成演示已经完成。

### 步骤 3 实际验证（2026-10-12）

本项目 Go 1.26.7，Go 命令均设置 GOTOOLCHAIN=local、GOPROXY=off、GOSUMDB=off。初次格式化发现 runner.go 有重复的 result 标识符造成语法错误，修正后完成以下检查；已有中文学习注释保留。

| 实际命令/检查 | 结果 |
| --- | --- |
| `gofmt -w ./internal/agent` | 完成，后续 gofmt -l 无输出 |
| `go test ./internal/agent -v` | 31 个测试函数及子测试通过，包耗时 0.727 秒 |
| 最终 `go test ./internal/agent` | 通过，0.765 秒；包含随后补充的错误返回用量测试及配置上限检查 |
| `go test ./...` | 全部 7 包通过，使用缓存结果 |
| `go build ./...` | 通过 |
| `git diff --check` / agent Go 文件 SPDX/LF 检查 | 通过 |

本步没有网络请求、Key 配置、Git 写操作或参考代码修改，未修改 CLI/现有工具接口；前三天及 Mock/Runner 历史测试未回归。README 已更新串行调度进度；正式真实 Git + Mock 演示仍待步骤 4。

下一步为步骤 4 的真实 Git + Mock 演示与统一验收；本次不接 CLI、signal.NotifyContext、真实模型、renderer 或 JSONL。
