# 第二天设计与验证记录

日期：2026-10-10。对应 [plan2.md](../plan2.md) 的 Git 输入层与步骤 6 集成验收。

## 固定版本的数据流

```text
repo + base ref + head ref
    → Resolve(ctx, repo, base, head)
    → Snapshot{RepoRoot, BaseSHA, HeadSHA}
    → Load(ctx, snapshot)
        → GetChanges：固定 SHA 之间的文件列表和逐文件 RawPatch
        → staticPathReason：路径/类型筛选
        → ParsePatch：Lines、AddedLines、Insertions、Deletions
        → ReadHeadFile：从固定 head 树核对模式、大小并读取 blob
        → 内容筛选 → applyReviewLimits
    → ChangeSet{Snapshot, Files, Warnings, HasCoverageGap} + error
```

直接比较两个提交树，不计算 merge-base。`Resolve` 支持仓库子目录，验证非 bare 仓库与 commit 引用，返回完整 SHA；默认 `HEAD~1` 在根提交上失败时要求显式提供基线。两端解析不是原子操作，但 Snapshot 返回后，分支移动、暂存和工作区编辑不会改变其内容来源。

文件清单由 NUL 分隔的状态/路径对解析并排序，不从 diff 文件头猜路径。Git 参数逐项传给 `os/exec`；逐文件 patch 使用字面 pathspec，禁用重命名识别、外部 diff、textconv 和颜色。重命名对应旧路径 D、新路径 A。`ReadHeadFile` 用 `ls-tree` 检查固定 head 中的普通 blob、模式和大小，再通过对象 ID 读取内容，不读取工作区文件或跟随其链接。每次 Git 操作设置 10 秒超时，继承取消，错误附操作名和有界 stderr。

`ChangeSet.Files` 保留被排除的文件。早期 Git/解析/读取错误返回空 ChangeSet；整体范围过大则返回已加载的 ChangeSet 与 `ErrRangeTooLarge`。调用者必须检查 error，不能把这类结果当成完成的审查。

## 演示与行号计算

`TestLoadDisplaysNumberedDiff` 在临时仓库中创建两次提交。base 中第 4 行为 `return a - b`；head 在此处加入注释，并改为 `return a + b`。实际测试还留下不同的暂存和工作区内容，以核对 Load 始终读取提交对象。

```text
  base:1 head:1 package sample
  base:2 head:2
  base:3 head:3 func Sum(a, b int) int {
- base:4         return a - b
+ head:4         // Add both inputs.
+ head:5         return a + b
  base:5 head:6 }
AddedLines: [4 5]
```

上例缩进仅作展示。测试输出保留原始代码缩进，并打印文件路径及本次 base/head 的完整 SHA；提交 SHA 会随 fixture 创建时间变化。

上下文递增两侧行号；删除仅递增 old；新增将当前 new 行号加入 AddedLines，再递增 new。不存在的一侧用 0 表示。每个 hunk 重新初始化计数；文件头 `+++` 和无末尾换行标记不消耗代码行号。该演示断言 2 行新增、1 行删除、AddedLines 恰为 `{4,5}`，并把所有正文行与对应 base/head 内容逐项比较。另有真实多 hunk 测试验证新增锚点 `{2,3,26}`；纯 patch 表格 fixture 覆盖 CRLF、缺省 count、零 count、空文件和正文前缀，无需新增外部 fixture 文件。

## 筛选原因和覆盖范围

| 原因 | 含义 |
| --- | --- |
| `deleted` | 删除文件，不读取 head |
| `invalid_path` | 不安全路径或控制字符 |
| `unsupported_extension` | 非 `.go` 文件 |
| `excluded_directory` | 路径段为 vendor 或 testdata，允许 myvendor 等名称 |
| `generated` | 固定 head 内容含标准 `// Code generated … DO NOT EDIT.` 标记 |
| `binary` | patch 二进制标记或正文含 NUL |
| `non_utf8` | 正文不是有效 UTF-8 |
| `special_file` | 符号链接、子模块等非普通源码 |
| `no_added_lines` | 空文件或纯删除等没有新增锚点的变更 |
| `head_too_large` | head 内容超过 256 KiB，记覆盖缺口和 warning |
| `patch_too_large` | patch 超过 32 KiB，记覆盖缺口和 warning |
| `range_too_large` | 候选超过 8 个文件或新增加删除超过 400 行，整体报错 |

静态筛选后，候选以及 `head_too_large` 文件计入整体预算；先检查整体限制，再应用单文件 patch 限制，避免大文件被跳过后绕过预算。恰好 8 文件、400 行、32 KiB patch 和 256 KiB head 内容允许。静态排除不产生覆盖警告；大小限制会产生覆盖缺口；整体超限不会偷偷选择前几个文件。`Reviewable` 表示输入适用性，最终 Agent 的 partial/failed 状态尚未实现。

## 测试覆盖

| 测试 | 核对内容 |
| --- | --- |
| `TestLoadChangeKindsAndSpacePaths` | Load 的普通修改、新增、空格路径、重命名 D/A、内容及统计 |
| `TestLoadKeepsAllFilesAndReasons` | 删除、生成、二进制、UTF-8、目录、空文件、链接/子模块及大小缺口 |
| `TestResolvePinsCommitsFromSubdirectory` | 子目录解析、固定 SHA、分支移动、读取不修改工作区/索引 |
| `TestResolveInvalidInput` / `TestGitCancellation` | 无效引用、根提交默认基线失败、取消和过期 deadline |
| `TestGetChangesAndReadFixedHead` | 字面路径、工作区文件修改/删除及分支移动后仍读取固定 head |
| `TestReviewLimitsBoundariesAndOrdering` | 小型内存 fixture 验证 8/9、400/401、32 KiB 边界及限制顺序 |
| `TestLoadRejectsWholeOversizedRange` | 真实 Load 整体超限，不缩减集合 |
| `TestLoadDisplaysNumberedDiff` | 真实 Load 演示、AddedLines 与两侧内容核对、暂存/工作区保持不变 |

`newRepo` 使用 `t.TempDir()`，隔离系统/全局 Git 配置及系统 attributes，使用空模板，显式初始化测试自己的 main 分支，配置局部身份、禁用签名和 hooks、设置 `core.autocrlf=false`。链接及子模块使用 `update-index --cacheinfo` 构造，不依赖 Windows 符号链接权限。测试只在临时仓库中创建提交，不对用户或参考仓库执行写 Git 操作。

## 实际验证结果

2026-10-10，在 Windows/amd64、Go 1.26.7、Git 2.45.1.windows.1 环境完成以下检查。Go 验证进程设置 `GOTOOLCHAIN=local`、`GOPROXY=off`、`GOSUMDB=off`，使用本机工具链，不下载模块或访问校验服务；项目仅依赖标准库。

| 检查 | 结果 |
| --- | --- |
| `gofmt -w ./internal/gitdiff` | 已格式化；已有中文学习注释保留 |
| `go test ./...` | 全部 5 个包通过，包含第一天测试；gitdiff 用时约 26 秒 |
| `go build ./...` | 通过 |
| `go test ./internal/gitdiff -run '^TestLoadDisplaysNumberedDiff$' -v` | 通过；打印 sample.go、两端 SHA、带行号 patch，AddedLines 为 `[4 5]` |
| gitdiff 全部 `.go` 文件的 SPDX/LF 检查 | 通过，无 CR 字节 |
| `git diff --check` | 通过 |

此外，在系统临时目录构建 `mini-review.exe`，直接启动实际进程（未使用 `go run`）核对第一天 CLI 行为：

| 参数 | 退出码 | stdout | stderr |
| --- | --- | --- | --- |
| `--help` | 0 | 空 | 帮助文本 |
| `--format=text` | 2 | 空 | 参数错误 |
| `--repo . --base main --head HEAD --format json --mock` | 1 | 空 | `Review execution is not implemented yet.` |

本次未发起网络请求、模型调用或审查报告生成；Git 写操作仅用于临时测试仓库的 fixture，不修改用户仓库的索引或被审查工作区，也未修改参考项目。开发改动是本项目测试与文档，以及对 gitdiff 源码执行格式化；保留任务开始时已有的本地实现。

## 后续接口

第二天提供固定 head 内容、逐文件 patch、新增锚点和筛选/覆盖结果。第三天可以据此实现 file_read、Finding 锚点校验及工具契约。当前 CLI 仍只解析参数；合法参数返回 `Review execution is not implemented yet.`。Agent、Runner、模型、报告和 trace 继续留给后续步骤。
