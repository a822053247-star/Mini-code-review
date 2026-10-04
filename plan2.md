# 第二天开发计划：Git 输入、Diff 解析与行号映射

依据：[需求文档](mini-code-review-agent-requirements.md) 第 5 节、第 7.1 节、第 10 节，以及第 11 节的第二天安排。

工作目录：`E:\study\Mini-code-review`。参考项目：`E:\study\alibaba`。

**当天目标：约 4 小时，得到固定 base/head 提交之间的结构化变更，并能打印带新行号的 patch；关键解析和 Git 集成测试通过。**

第一天已完成参数解析、审查结果结构和模型客户端契约。模型文件当前为 `internal/model/review.go`／`review_test.go`，继续沿用，不重新创建重复结构。

本计划分为 6 个子任务。第二天不实现 Agent、工具调用循环、真实模型、报告 renderer 或 trace。带行号 diff 通过开发测试输出验证，现有 CLI 仍保持“合法参数但审查未实现”的行为，不伪造最终报告。

## 步骤 1：阅读 Git/Diff 链路，确定输入契约（25 分钟，不写代码）

**阅读内容**

- 需求第 5 节：直接比较、固定 SHA、文件筛选、范围限制。
- `internal/diff/git.go`：阅读参考项目的 Provider、输入解析和获取变更逻辑，只关注范围模式。
- `internal/diff/parser.go`：关注每个文件 patch 的解析、二进制识别、CRLF 处理。
- `internal/diff/hunk.go`：关注 hunk 起点和行号递增规则。
- `internal/model/diff.go`：观察变更模型如何保存 patch、文件路径和代码内容。

以上源码路径相对于 `E:\study\alibaba`。不照搬工作区、merge-base、重新定位、会话恢复等能力。

**操作与产物**

只阅读和写 `notes/day2-design.md` 初稿，明确以下约定后再开始编码：

1. 比较 base 与 head 的两个提交树，不自动计算 merge-base。
2. 启动时解析并固定 SHA；后续所有操作只使用固定值。
3. 文件路径来自 NUL 分隔列表，每个文件单独取 patch，不从 diff 头的空格中猜路径。
4. 普通修改、新增可审查；删除和其他排除文件仍保留在变更清单中。
5. 新增行的行号属于 head；删除行的行号属于 base。

**完成标准**

能手算一段含新增、删除和上下文的 patch：说明哪种行递增哪一侧计数，为什么 `+++` 文件头不属于新增代码，为什么修改工作区不应影响本次结果。

## 步骤 2：实现 Git 调用与提交版本固定（35 分钟）

**准备编写**：`internal/gitdiff/git.go`、`internal/gitdiff/types.go`；测试随实现添加到 `git_test.go`。

**实施内容**

- 用标准库 `os/exec` 调用 Git，参数逐项传入，设置工作目录；不使用 PowerShell、cmd 或 shell 拼接命令。
- 集中封装 Git 调用，接收 context，每次请求设 10 秒超时并继承上游取消；错误需保留操作名称与有界 stderr。
- 确认 Git 可用、repo 位于有效的非 bare 工作仓库，并解析仓库根目录。允许从仓库子目录启动，输入不存在或不是仓库时明确报错。
- Git 层也拒绝空白和以 `-` 开头的 ref，不能假设所有调用者都经过 CLI 校验。
- 将 base/head 验证为 commit 并解析完整 SHA，固定到一个 Snapshot 值；使用 Git 的选项结束机制，避免输入被解释为选项。
- 根提交缺少默认 `HEAD~1` 时返回错误，建议显式指定有效基线；不自动切换为空树或其他模式。
- 提交解析只进行一次。后续分支移动或工作区修改不会改变该 Snapshot 的内容来源。

**建议最小类型与入口**

| 名称 | 职责 |
| --- | --- |
| `Snapshot` | 保存 RepoRoot、BaseSHA、HeadSHA，作为本次 Git 输入的固定身份 |
| `Resolve` | 接收 context、repo、base、head，返回 Snapshot 或 error |
| 内部 Git 调用函数 | 执行参数数组，处理超时、取消和 stderr |

Snapshot 是普通值类型，不设计缓存、仓库管理器或通用插件接口。第一天的 `model.Report` 仍只负责最终结果。

**完成标准**

临时仓库中的两个提交能得到固定 SHA；不存在的 ref、非法 ref、非仓库和已取消 context 都返回错误，未修改仓库工作区或索引。

## 步骤 3：取得逐文件 Patch 与 head 内容（40 分钟）

**准备修改**：`internal/gitdiff/git.go`、`types.go`、`git_test.go`。

**实施内容**

- 在固定 SHA 之间取得 NUL 分隔的文件状态/路径列表，支持新增、修改、删除和类型变化；按路径排序。
- 关闭 rename detection，重命名按删除旧路径和新增新路径处理。
- 根据已经取得的单个路径生成普通 unified patch，关闭外部 diff、textconv、颜色和 rename detection，保留少量上下文。
- 路径作为字面 pathspec 传入；只添加 `--` 还不够，不能让合法路径中的通配符扩大匹配范围。
- patch 和文件列表都用固定 BaseSHA/HeadSHA，不再使用原始分支名。
- 增加按 Snapshot.HeadSHA 读取普通文件内容的入口，为第 3 天的 `file_read` 提供基础。内容从 Git 对象取得，不读取工作区文件、不跟随工作区符号链接。
- 读取前核对 head 树中的对象类型与文件模式；符号链接、子模块等不能当作普通源码读取。路径检查拒绝绝对路径、盘符、`..` 路径段和控制字符。
- 普通空格路径必须支持；路径统一保留 Git 的 `/` 表示。特殊路径不要靠逐行切分或简单去引号处理。

**建议最小契约**

| 类型/入口 | 数据或行为 |
| --- | --- |
| `FileChange` | Path、变更类型、RawPatch；后续补解析行及统计 |
| `ReadHeadFile` | 根据 Snapshot 和仓库相对路径读取内容；限制单文件 256 KiB |
| 获取变更的内部函数 | 返回逐文件变更，包括以后需要记录的排除项 |

`ReadHeadFile` 当天只完成版本读取、普通文件检查与大小保护；英文工具说明、JSON Schema、行范围和 200 行/16 KiB 返回限制在第 3 天实现。

**完成标准**

同一测试仓库包含空格路径、新增、修改和重命名时，取得的文件列表与 patch 正确。提交后修改或删除工作区文件，读取 head 内容仍与提交一致。

## 步骤 4：实现 Hunk 解析与新增行号映射（50 分钟）

**准备编写**：`internal/gitdiff/parser.go`、`parser_test.go`；扩展 `types.go`。

**实施内容**

- 解析 `@@ -oldStart,oldCount +newStart,newCount @@`，支持省略 count（默认 1）及 count 为 0 的情况。
- 遇到每个 hunk 重新初始化 old/new 行号；普通上下文、新增、删除分别按下表处理。
- 在 hunk 外忽略文件头等元数据；在 hunk 内按首字符识别内容，不能把所有以 `+++` 开头的行都当作文件头。
- `\ No newline at end of file` 不消耗行号；解析 CRLF 时只处理行尾 `\r`，不任意改写代码正文。
- 保存新增行集合 `AddedLines`（可用 `map[int]struct{}`）、新增/删除数量、每条 patch 行的新旧行号及正文。
- 二进制标记单独识别，不当作文本 hunk；遇到坏 hunk 或计数不匹配时返回错误，不产出看似合法的错误行号。
- 提供简单的开发展示函数或测试辅助函数，显示 `+ head行号`、`- base行号` 和上下文。尚不接入最终报告格式。

| 行类型 | old 计数 | new 计数 | AddedLines |
| --- | --- | --- | --- |
| 上下文（空格前缀） | +1 | +1 | 不加入 |
| 新增（`+` 前缀） | 不变 | +1 | 加入当前 new 行号，再递增 |
| 删除（`-` 前缀） | +1 | 不变 | 不加入 |
| 文件头/无末尾换行标记 | 不变 | 不变 | 不加入 |

**完成标准**

覆盖多 hunk、新增文件、删除文件、空文件、CRLF、无末尾换行、缺省 count，以及代码正文以 `+`/`-` 开头的场景。新增锚点行号与 head 内容一致，不把文件头计入统计。

## 步骤 5：组合输入加载、筛选与范围限制（45 分钟）

**准备编写**：`internal/gitdiff/load.go`、`load_test.go`；按需要补充 `types.go`。

**实施内容**

- 提供 `Load` 入口，接受 context 与 Snapshot，组合逐文件 patch、head 内容、parser 和筛选结果。
- 返回一个 `ChangeSet`：固定版本、所有文件变更、Warnings；每个 FileChange 保存可审查标记和 SkipReason，避免静默丢弃文件。
- 静态排除：删除、非 `.go`、`vendor/` 或 `testdata/` 目录、标准生成标记、二进制、非 UTF-8、符号链接/子模块等特殊类型、含控制字符的路径。
- 目录排除按路径段判断，不用简单子串匹配误排 `myvendor/` 等正常目录。
- 生成标记从固定 head 内容识别。为空或仅删除行、没有 AddedLines 的变更可保留为 `no_added_lines` 排除项；不要伪造可以提交问题的锚点。
- 静态筛选后的候选集合，最多 8 个文件、合计最多 400 行新增/删除。先检查整体限制，再处理单文件 patch 限制，防止通过跳过大文件绕过整体保护。
- 整体超限返回明确错误并建议缩小范围；恰好 8 个文件或 400 行允许。单文件 patch 大于 32 KiB 时记录 `patch_too_large` 和 warning，其他文件继续保留。
- head 文件超过读取上限时记录独立的大小限制原因和 warning，不冒充静态排除。删除文件不用读取 head。
- 当天保留覆盖缺口与 warning 数据；`partial`/`failed` 的最终运行状态由后续 Agent 汇总，不在 Git 层假定审查完成。

**完成标准**

所有文件都有可解释的入选/排除结果。超限不会只取前几个文件；零适用文件与因大小限制无法覆盖的情况能区分。输入层不依赖 `internal/cli`、`internal/llm` 或工具注册表。

## 步骤 6：集成测试、打印 Diff 与记录成果（45 分钟）

**准备完善**：上述测试、README 和 `notes/day2-design.md`；纯 patch 样例可存入 `testdata/diff/`。

**测试与演示**

1. 用 `t.TempDir()` 建临时仓库，为其配置局部用户身份，显式关闭测试仓库的自动换行转换，再创建两次提交；不依赖真实仓库的分支名、用户配置或未提交修改。
2. 验证普通修改、新增、空格路径、重命名、删除、生成文件和二进制的结果；符号链接可通过树对象/索引 fixture 构造，避免依赖 Windows 创建链接的权限。
3. 验证 repo 子目录解析、无效 ref、默认基线在根提交上失败、已取消 context，以及工作区修改后仍读取固定 head。
4. 验证 8/9 文件、400/401 行，以及 32 KiB 边界和超限；范围判定尽量用小型数据 fixture 测试，不为每个边界都创建大量 Git 提交。
5. 增加一个明确命名的演示测试，例如 `TestLoadDisplaysNumberedDiff`，调用真实 Load 后用 `t.Logf` 打印文件路径、SHA、带行号 patch 和 AddedLines。
6. 格式化源码，运行全部测试与构建，再运行演示测试查看行号；现有第一天测试也必须继续通过。

```powershell
Set-Location E:\study\Mini-code-review
gofmt -w .\internal\gitdiff
go test ./...
go build ./...
go test ./internal/gitdiff -run '^TestLoadDisplaysNumberedDiff$' -v
```

**文档记录**

- README 区分“Git 输入可在测试中运行”与“CLI 已能完整审查”，不能将后者标为完成。
- day2-design 记录固定版本的数据流、行号计算例子、筛选原因和实际验证结果。
- 新源码加 SPDX 头，保持 LF；新标识符、诊断字符串使用英文，已有中文学习注释按既有偏好保留。
- 未建立 Makefile 时继续使用本项目 Go 工具链；不调用参考仓库 Makefile、不提交或修改参考项目代码。

**完成标准**

测试与构建通过，演示输出的新增行号能与提交内容逐一核对；现有 CLI help/退出码行为不变；没有网络请求，也没有修改用户仓库的工作区或索引。

## 当天交付与验收

预计新增文件（按实际拆分保持精简）：

```text
Mini-code-review/
├── internal/gitdiff/
│   ├── types.go
│   ├── git.go
│   ├── git_test.go
│   ├── parser.go
│   ├── parser_test.go
│   ├── load.go
│   └── load_test.go
├── testdata/diff/          # 仅在需要文件 fixture 时新增
└── notes/day2-design.md
```

- [ ] base/head 解析为固定 commit SHA，直接比较，无 merge-base 推断。
- [ ] 普通空格路径正确处理，Git 参数不经 shell 拼接，pathspec 使用字面匹配。
- [ ] head 内容读取与 patch 来源一致，未提交修改不影响结果。
- [ ] 多 hunk 的新旧行号、AddedLines 和统计正确。
- [ ] 静态排除、大小覆盖缺口和整体超限具有不同原因。
- [ ] 全部测试、构建和带行号演示通过，第一天行为未回归。
- [ ] 设计笔记记录实际成果，能向第 3 天提供固定 head 内容和新增锚点集合。

如果进度落后，停止额外抽象和展示美化，优先完成版本固定、普通文本解析与关键测试。未实现的筛选或边界须明确记录，不标为完成；工具、模型和最终报告仍留到对应日期。
