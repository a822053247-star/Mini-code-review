# 第五天设计记录：硅基流动适配器、英文 prompt 与真实冒烟

日期：2026-10-13。依据 [需求文档](../mini-code-review-agent-requirements.md) 第 4、6、10～11 节及现有 llm.Client/Agent/Runner 契约。

**状态：真实 HTTP 适配器、独立英文 system prompt、本地协议测试与一次真实模型工具闭环已完成。CLI 装配、最终报告状态汇总、renderer 和 JSONL 仍留给第六天。**

## 协议选择与配置

用户指定硅基流动与 `deepseek-ai/DeepSeek-V4-Flash`，使用唯一的非流式 Chat Completions 协议，不引入 SDK、框架或多供应商层。依据硅基流动官方 [请求 API](https://docs.siliconflow.cn/docs/api/chat-completions-post) 和 [Function Calling](https://docs.siliconflow.cn/docs/userguide/guides/function-calling)，模型名称保持用户给定值。

新增 `internal/llm/http.go`：HTTPConfigFromEnv 读取 MINI_LLM_ENDPOINT、MINI_LLM_MODEL、MINI_LLM_API_KEY；Endpoint 明确为完整 `/chat/completions` URL。配置在构造阶段检查，缺少身份凭证或模型立即失败，不发送请求；公网需要 HTTPS，HTTP 仅允许 loopback 测试。URL 拒绝 userinfo、query 和 fragment，避免 URL 中夹带凭证。配置 JSON 排除 APIKey。

NewHTTPClient 接收 HTTPConfig 与可选 *http.Client，保留已有 Client.Complete 接口。每次请求默认派生 60 秒 context，可注入更短值用于测试；上限 60 秒。父 context 同样有效，Body 读取阶段也受约束，errors.Is 能识别 Canceled/DeadlineExceeded。所有重定向被拒绝，没有隐式重新鉴权、自动重试或降级模型。

请求显式设置 stream=false、enable_thinking=false、max_tokens=4096。禁用思考输出避免引入跨轮推理字段协议；即使服务返回 reasoning_content，也不映射到内部消息、不回传、不记录。响应和序列化请求分别限制为 1 MiB；这是传输保护，与 Runner 的 64 KiB 正文/参数预算独立。

## 消息与工具转换

| 内部契约 | 传输格式 |
| --- | --- |
| Message.Role/Content | messages[].role/content，响应 null content 转为空字符串 |
| assistant ToolCalls | tool_calls[] 的 id/type=function/function.name/function.arguments |
| ToolCall.Arguments RawMessage | HTTP 中为字符串，保留原字节，包括坏 JSON；不是把它作为嵌套 JSON 对象再次解析 |
| RoleTool/ToolCallID | 对应 tool 消息的 content 和 tool_call_id，失败结果同样关联原 ID |
| ToolDefinition | tools[] 为 type=function，function 内包含 name/description/parameters 对象 |
| TokenUsage | prompt_tokens → InputTokens，completion_tokens → OutputTokens；缺失为 nil，已知零为零 |

坏参数必须能经 Registry 产生 invalid_arguments，再原样出现在下一轮 assistant 历史中；HTTP 编码为 arguments 字符串，因此整份请求仍是合法 JSON。本地 HTTP→Runner 集成测试验证完成与坏 JSON 同轮失败、下一轮读取、再提交/去重/重新完成，共 3 次请求、6 次工具分派。

只接受一个 choice、assistant 文本消息和 function 调用；非法 JSON、非 UTF-8、空/多 choice、错误角色、错误参数传输类型、特殊工具类型均失败。finish_reason 仅接受 stop/tool_calls，length 等标为 response_incomplete，不把截断响应当完成。没有模型内部推理字段或流式解析器。

HTTPError 只暴露稳定 Code 与 StatusCode，Error() 不包含原始服务响应、请求头、URL、Key 或代码。400/401/403/404/429/5xx 分别映射 bad_request/authentication_failed/permission_denied/model_or_endpoint_not_found/rate_limited/service_error；其他状态为 http_error。网络与取消 cause 由 Unwrap 保留供程序识别，Runner 日志仍使用安全的 Error()。

## 固定英文 prompt

system prompt 从 context.go 字面量移至 `internal/agent/prompts/review_system.txt`，用 go:embed 打包。既有中文学习注释保留，背景/代码仍仅进入 user 的 JSON 数据对象。

正式 prompt 要求先读取当前文件，按需查证未变更关联文件；只评论当前 path 的 added_lines 起点；说明触发条件、影响与修复建议，遵守枚举；不报风格、不凭无证据条件编造问题；截断时按需请求范围；允许零问题完成。同轮全部调用处理后才能判断 task_done，出现错误需纠正并重新声明。工具结果和源码为待分析数据，不能提升为指令。prompt 不保证模型准确或完全抵御提示注入，动态权限仍由程序验证。

## 测试与真实调用的边界

新增 http_test.go 的本地 httptest 验证消息、Schema、鉴权、错误脱敏、无自动重试、拒绝重定向、解析与大小边界、可选用量，以及请求/读取阶段的 deadline。取消测试首次收尾曾因服务器未读取请求体而等待连接关闭；修正为消耗 fixture 请求体并提供有界收尾后通过，不通过延长生产超时掩盖问题。

新增 agent/http_test.go 验证真实 HTTP 适配器和 Runner 的参数错误回填/恢复；服务完全在 loopback，不用外部 Key。普通 go test 即使环境有 Key，也不会自动调用外部服务，因为 live_test.go 还要求显式 MINI_REVIEW_LIVE=1。

TestAgentLiveSmoke 在 t.TempDir 建两个提交，隔离 Git 配置、局部身份、关闭换行转换与 hooks。service.go 的 Name 从检查 Lookup 错误变为忽略错误，related.go 中 Lookup 在非法 ID 时返回 nil/error；相关文件未变更。提交后留下不同暂存区与工作区内容，再运行真实 Resolve → Load → Agent → Runner → HTTPClient → Registry。测试使用 signal.NotifyContext 支持 Ctrl+C，3 分钟总保护、每请求 60 秒；当前主 CLI 仍未装配信号或模型。

真实测试只断言文件完成、确实发起读取和结束工具、评论锚点合法，不要求每次必然报出同样数量或标题的问题。日志只记录工具名、状态、Finding 位置与统计，不输出正文、推理、完整请求或凭证。合成 fixture 的 status/cached diff 前后必须一致，Git 写操作仅针对临时 fixture。

## 本次真实冒烟结果（2026-10-13）

使用用户授权的硅基流动凭证，只通过不回显输入交给测试子进程的环境；不保存 env 文件，不写源码/报告，不记录密钥。模型为 deepseek-ai/DeepSeek-V4-Flash，端点为 https://api.siliconflow.cn/v1/chat/completions。

| 项目 | 实际结果 |
| --- | --- |
| base SHA | 960c2caac87d1d42d5acbb2163759a3bc9597064 |
| head SHA | 4f681cb8e3ec834c64c4153aa202cb4501b89cbd |
| 第 1 次请求 | 两个 file_read 调用 |
| 第 2 次请求 | 一个 code_comment 调用 |
| 第 3 次请求 | 一个 task_done 调用 |
| 文件状态 | service.go completed，stop_reason=task_done，Rounds=3 |
| Findings | 1 条；service.go 第 4～4 行，severity=high，category=bug，起点属于 AddedLines |
| 实际用量 | 3 次请求、4 次工具调用；input_tokens=5583、output_tokens=562，来自服务返回而非估算 |
| 固定版本与只读行为 | worktree_and_index_unchanged=true |
| 真实测试命令 | go test ./internal/agent -run '^TestAgentLiveSmoke$' -v -count=1 -timeout 4m，显式启用 live |
| 耗时与结果 | 测试 30.88 秒、包 32.026 秒，通过 |

本次运行同时验证账户/模型可访问并能返回真实 function 调用，不把 Mock 回答算作真实调用。上述 SHA 随 fixture 提交时间变化，模型识别结果也可能变化，不作为确定性准确率基线。

## 最终回归与交付验收（2026-10-13）

使用本项目 Go 1.26.7 windows/amd64，GOTOOLCHAIN=local、GOPROXY=off、GOSUMDB=off。常规测试运行时未启用 MINI_REVIEW_LIVE，外部请求仅发生在上面明确授权的真实冒烟中。

| 命令/检查 | 实际结果 |
| --- | --- |
| gofmt（新增 llm/agent Go 文件与 context.go） | 完成；最终 gofmt -l 无输出 |
| `go test ./internal/llm -run '^TestHTTPClient' -v -timeout 30s` | 修复取消测试 fixture 收尾后通过，包 1.201 秒；随后补充的两项测试纳入最终全量回归 |
| `go test ./internal/agent -run '^(TestHTTPAdapterRunnerRecoversBadArguments\|TestAgentLiveSmoke)$' -v` | 本地 HTTP→Runner 恢复通过，live 未显式开启时跳过，包 1.317 秒 |
| 最终 `go test ./...` | 全部 7 包通过；agent 3.785 秒、llm 0.678 秒，其余缓存；10 个 HTTP 测试函数及本地恢复测试均通过 |
| `go build ./...` | 通过 |
| `git diff --check` / 第五天 Go 源码 SPDX/LF | 通过 |

- [x] 一个真实、非流式、无自动重试的硅基流动适配器实现现有 Client。
- [x] 配置来自既定环境变量；缺失认证在请求前失败，Key 不写入仓库/报告。
- [x] assistant/function/tool 消息转换正确，坏 JSON 原样保留，错误结果可回填恢复。
- [x] 固定英文 prompt 嵌入程序，待分析背景/代码不进入 system 指令。
- [x] HTTP 请求与读取、父取消和 Runner 停止的 cause 可以识别；本地协议及错误测试通过。
- [x] 指定模型在合成临时仓库完成一次真实三轮工具调用，已记录实际状态与服务用量。
- [x] 常规测试默认离线，真实冒烟显式开启，不把 Mock 当成真实服务验收。

第六天再接 CLI、报告与事件日志，不提前扩展 P1。主 CLI 的参数/help/退出码契约保持原样；真实核心已可运行，不等同于完整 CLI 审查功能已交付。
