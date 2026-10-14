# 第六天设计记录：报告、JSONL、状态汇总与 CLI

日期：2026-10-14。依据需求第 4、8、10～11 节。第六天实现 CLI → Resolve/Load → Agent/Runner → 工具 → Report/JSONL 的装配，默认验收使用离线 Mock，不重复消耗真实模型额度。

## 包职责与装配

- internal/report：Build 接收 Agent.ReviewResult，集中计算 RunStatus；Render 生成 Markdown/JSON 字节，不执行模型或写文件。
- internal/trace：类型化 Event 与串行 JSONL Logger，只接收执行元数据；Sink 函数供 Agent/Runner 注入，零值不记录。首个写错误为 sticky trace.Error，保留 cause，Error() 不回显底层私有文本。
- Agent：记录 file_start/file_end，任务和静态 skipped 项都可核对；日志错误停止全局并保留已验证问题，未调度任务为 failed/trace_error，零轮次。
- Runner：记录 llm_start/end、tool_start/end，包含文件、轮次、调用关联、耗时、稳定错误码和实际返回用量。开始日志失败不执行对应调用，结束日志失败不回滚此前合法 collector 写入。
- cmd/mini-review：参数、信号、输出预检、客户端选择、run_start/end、报告写入与退出码。main 使用 signal.NotifyContext；数据/模型核心不调用 os.Exit。

没有新增模型框架或生产仓库管理器，继续复用现有接口。真实模式使用第五天硅基流动 HTTP 适配器与 MINI_LLM_* 环境；没有适用文件时不初始化模型配置，也不要求 Key。

## 状态与报告契约

状态优先级：输入/配置/日志/输出等 fatal 错误为 failed；无覆盖缺口且没有适用文件为 skipped；所有适用文件完成且无缺口为 completed；存在缺口但至少一个文件完成或已有合法 Finding 为 partial；其余为 failed。

静态排除保留 FileSkipped 与原原因，不算缺口；head/patch 大小跳过保留 Load 的 warnings/HasCoverageGap，唯一候选过大为 failed。全局取消保留已接受问题和完成文件，按进展为 partial 或 failed。发现问题本身不会改变 completed 或退出码。

JSON 保持既有 model.Report 字段，files/findings/warnings 始终为非 nil 空集合；token 未知为 null，已知零为 0。Build 复制可变结果与 token 指针。Markdown 呈现 SHA、状态、文件覆盖/原因/轮次、Finding 位置/严重度/触发影响/建议、warnings、用量和耗时；不完整运行明确写 Review incomplete，只有 completed 且零问题可使用完成的零问题说明。模型文字按文本转义，控制字符不会直接影响终端。

## JSONL 与失败处理

八类基本事件为 run_start、file_start、llm_start、llm_end、tool_start、tool_end、file_end、run_end。Logger 统一生成 run_id 与 UTC RFC3339Nano 时间；结束事件记录 duration_ms，工具结果以 tool_call_id 关联请求，失败含稳定 code。LLM 结束事件仅保留服务返回的可选 TokenUsage，不把缺失伪装成零。

Event 没有请求体、参数、代码、headers、Key、推理或原始错误字段。未知工具名记录 unregistered，错误仅记录程序错误码。日志是进程内串行写入，无网络遥测。普通模型/工具错误继续遵循已有恢复规则；trace 写失败升级为 fatal，不被局部失败继续机制吞掉。

run_end 在发布报告前记录并 flush，从而终结事件写失败时可以发布 failed 报告。run_end 描述审查阶段的最终状态；如果之后报告设备写入失败，追加 output_error（若设备仍可写）并返回 1，不能声称报告交付成功。对于损坏的 stdout/磁盘，无法保证完整报告或完整日志，stderr 与退出码仍尽量表明失败；不通过继续调用模型补救 I/O。

输出/日志文件用 O_CREATE|O_EXCL 预留，0600；已存在文件、同路径、不可写父目录在模型调用前失败，不截断、不静默覆盖。第二个文件预留失败时，只清理本次创建的空报告，并核对文件身份。--output 时 stdout 保持为空；普通 stdout 只写完整报告字节，诊断写 stderr。报告/日志 Close 或 Sync 错误同样返回执行失败。

## CLI 与 Mock 行为

退出码 0 为 completed/skipped，1 为 partial/failed 或 I/O 错误，2 为参数错误；help 为 0。旧的“合法参数也返回未实现”占位行为被真实运行取代，第一天参数/help 契约不变。

--mock 采用按已加载文件路径隔离的内存脚本：file_read → code_comment → task_done。评论标题为 Mock protocol example，报告 warnings 明确声明其为协议例子，不是语义审查结论。每个文件选择真实 AddedLines 中最小锚点，仍经真实 Registry 校验。独立脚本保证某文件 max_rounds 提前停止后，下一个文件不会消费前一个文件剩余响应。默认 Mock 没有 Key 或外部请求。

## 验证与产物

自动化覆盖状态矩阵、JSON 空集合/未知用量、Markdown 不完整提示及转义、事件身份与关联、短写/写失败脱敏、trace 失败后保留 collector 并停止后续任务、CLI 正常/partial/failed/skipped、唯一候选大小跳过、取消保留 partial、输出路径冲突/已存在/缺失父目录、stdout 隔离、Mock 提前停止隔离及输出失败事件。

TestCLIDay6Demo 仅在显式设置 MINI_REVIEW_DEMO_DIR 时写演示产物；普通 go test 不写项目目录。所有 Git 写操作位于 t.TempDir 的两提交 fixture，不修改用户仓库索引或参考项目。真实模型测试默认跳过。

2026-10-14 实际验证使用项目现有 Go 工具链，设置 `GOTOOLCHAIN=local`、`GOPROXY=off`、`GOSUMDB=off`：

| 命令或检查 | 实际结果 |
| --- | --- |
| `gofmt -w ./internal/agent ./internal/trace`；检查其他新增包格式 | 完成；`gofmt -l` 无输出 |
| `go test ./...` | 9 个包全部通过；CLI 13.772s，Agent 4.211s，Trace 0.442s，其余使用通过的缓存 |
| `go build ./...` | 通过 |
| `go vet ./...` | 通过 |
| `git diff --check` | 通过 |
| `go test ./cmd/mini-review -run '^TestCLIDay6Demo$' -v -count=1`，指定全新 `MINI_REVIEW_DEMO_DIR` | 通过；正常退出 0，不完整退出 1，使用 `--output` 时 stdout 均为空 |
| 构建实际 exe 并检查 `--help`、`--format=text` | 分别退出 0、2；stdout 为空，帮助/诊断在 stderr |

演示文件位于 [examples/day6](../examples/day6/README.md)，由真实 Git fixture、CLI 装配和工具执行产生：正常 JSON 报告为 completed，2 条明确标记的脚本 Finding，6 次请求/6 次工具调用，对应 30 条 JSONL 事件；不完整 Markdown 为 partial，a.go 完成、z.go 模型失败，2 次请求/1 次工具调用，对应 12 条事件。已逐行解析两份 JSONL，核对单一 run_id、最终 run_end 状态与请求/工具计数。fixture 的固定 SHA 写入报告，不将测试脚本用量冒充真实 token 消耗。

当天报告、状态汇总、可选日志、CLI 接入和异常测试已完成。新增 Go 文件带 SPDX 头并保持 LF。第七天继续三个业务样例、真实 CLI 冒烟、自查与 README 验收；第六天没有发起真实模型请求，不扩展 P1。
