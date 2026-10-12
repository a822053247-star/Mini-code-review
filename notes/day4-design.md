# 第四天设计记录：Runner 契约与脚本 Mock

日期：2026-10-12。对应 [plan4.md](../plan4.md) 步骤 1，依据 [需求文档](../mini-code-review-agent-requirements.md) 第 6、8、10～11 节。

**状态：脚本 Mock 与测试已实现；Runner 契约在本文确定，Runner、串行调度、CLI 接入仍未实现。下面 Runner 签名是后续实施约定，不是现有可调用 API。**

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

## Runner 的后续输入输出约定

计划在 `internal/agent/runner.go` 实现以下形状，当前只记录设计：

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

构造阶段检查 client/registry 非 nil；options 零值使用 8 轮、64 KiB、60 秒默认值。MaxRounds 显式值限 1～20；上下文与超时显式值必须为正，可在测试中设较小限制，生产装配沿用需求默认值。负数为装配错误，不当成默认值。初始消息保存独立副本，每文件重新开始；工具定义来自 registry，不从 Mock 提供的文本获取。

Run 返回 FileResult（Path、Status、StopReason、Rounds）和本文件 Usage。完成为 completed/task_done；普通停止为 failed，并记录 invalid_response/max_rounds/context_limit。请求或工具不可继续的 Go error 同时返回失败结果与 wrapped cause，不丢失已有统计。父 context 取消优先检查 ctx.Err，通过 errors.Is 保留 cause；调度方据此停止全局。局部模型错误/请求超时则记录文件失败并继续下一文件。Runner 不调用 os.Exit。

Rounds 与 LLMRequests 统计实际发起的请求，包括失败请求；ToolCalls 统计实际开始分派的调用，包括普通失败、重复评论及 task_done。未执行的后续调用不虚增数量。token 输入/输出分别累加，任一次请求对应项未知则该项总量为 nil；零请求为已知零。全局汇总留给后续调度/报告层。

同轮完成规则在步骤 2 实施：先保存 assistant，串行执行全部普通调用，逐个回填 Result，最后检查合法 Completion 且同轮无错误。完成不能跳过后续评论，也不能沿用上一轮被拒绝的声明。这里尚未实现或验证该状态机。

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

本步未修改现有客户端/工具契约、CLI、参考代码或项目 Git 索引，没有创建报告/trace。前三天测试继续通过；Runner 完成判断与停止条件仍待后续实施，不能把 Mock 脚本单测说成完整 Agent 审查。

下一步为 plan4 步骤 2：实现初始上下文、消息循环、多工具结果回填与同轮完成判断。
