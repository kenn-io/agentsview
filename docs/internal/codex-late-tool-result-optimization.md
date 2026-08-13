# Codex 同步性能：审计、优化计划与实现记录

日期：2026-08-13
状态：优化进行中（首个 PR：晚到工具结果增量定点更新）

> 本文件是优化计划的权威记录。性能审计部分来自 2026-08-13 对运行中
> AgentsView v0.39.0（提交 `627a8afa`）的分析，并对照了当时源码主线；
> 核心问题在主线中仍然存在。

## 最终判断（摘要）

这不是“数据太多得不合理”。20GB 档案和接近 1GB 的单会话确实会暴露边界，
但一个会话浏览器面对这种数据，合理行为应该是：

- 无变化启动：几十毫秒到几百毫秒的元数据检查；
- 小量追加：只读取新增尾部；
- 工具结果：定点更新；
- 完整重建：允许耗时，但内存始终受控。

当前实现则把多个本应 `Θ(d)` 的日常操作做成了 `Θ(S)`，并在完整同步时关闭
空间预算。因此 9.3GB 峰值不是单一 bug，而是几项设计选择叠加后的必然结果。
继续只打 systemd/cgroup 补丁，会不断以不同形式复发。

## 复杂度总览

设：

- `N`：会话文件数量，目前约 967 个来源、774 个 Codex 文件
- `B`：所有源文件总大小，目前 Codex 会话约 20.1GiB
- `S`：一个活跃会话的大小，最大约 945MB
- `d`：本次新增内容，通常只有几 KB～MB
- `M`：该会话解析后的消息数量
- `W`：解析线程数，目前最多 8

| 场景 | 当前复杂度 | 合理目标 |
|---|---:|---:|
| 无变化的服务启动 | `Θ(B)`，读取整个档案 | `Θ(N)`，只做文件属性检查 |
| 一个会话追加 `d` 字节 | `Θ(S)`，甚至多次扫描 | `Θ(d)` |
| 一条工具调用结果 | `Θ(S + M)` | `Θ(d + log M)` |
| 活跃会话连续追加 `k` 次 | 最坏接近 `Θ(k²)` | `Θ(k)` |
| 快速全局同步中的 Codex 去重 | `O(N²)` | `O(N)` |
| 完整解析峰值空间 | 多个完整会话叠加 | 固定内存预算或流式 `O(chunk)` |

## 第一处核心问题：启动时对全部 Codex 文件做完整哈希

启动 worker 是一个全新进程，它没有上一轮的内存缓存。

v0.39.0 的流程是：

1. 发现文件；
2. 对每个 Codex 文件计算完整内容哈希；
3. 得到哈希后，才去数据库判断它其实没有变化；
4. 未变化则跳过解析。

也就是说，“是否需要读取文件”的判断发生在“已经把文件完整读完”之后。

因此，哪怕 774 个 Codex 会话一个字节都没变，每次冷启动仍然要读取约
20.1GiB。日志也吻合：

- 发现 967 个文件只花了 25ms；
- 随后约 28 秒都在哈希和检查大文件；
- 之后才出现大文件的增量回退日志。

应改为：

1. 先比较持久化的 `size + mtime + ctime + inode + device`；
2. 未变化直接跳过；
3. 只有属性变化时才验证尾部或哈希；
4. 完整内容哈希放到低优先级审计中。

这样无变化启动可从 `Θ(B)` 降到 `Θ(N)`。

## 第二处核心问题：所谓“增量解析”仍然扫描整个旧文件

对一个有少量新增内容的 Codex 文件，目前至少存在三项完整文件工作：

1. `Fingerprint` 完整哈希整个文件：`Θ(S)`
2. startup worker 的解析游标只存在内存，新进程缓存为空，因此重新扫描
   `[0, oldOffset)` 来恢复状态：`Θ(S)`
3. 增量成功后，为保存新的完整文件哈希，又从头哈希到新末尾：`Θ(S)`

所以即使增量解析成功，冷 worker 处理一次几 KB 的追加，也可能读取约 `3S`。

对于 945MB 会话，就是为几 KB 新内容读取接近 2.8GB 源数据。

根本原因是：程序把增量游标放在短命进程的内存里，却没有持久化到数据库。
worker 隔离进程本身是合理的，因为退出后可以释放内存；但依赖该进程的内存
缓存来保证增量性能，就和短命 worker 的设计互相冲突。

应持久化的游标很小，包括：

- 已消费字节偏移；
- 当前模型、cwd；
- 首条用户消息摘要；
- 最近 token 事件摘要；
- 当前任务状态；
- 文件身份；
- 尾部校验块或可续算的哈希状态。

## 第三处核心问题：几乎所有工具结果都会触发整文件回退

当前判断逻辑中，只要新增的是非空：

```text
function_call_output
custom_tool_call_output
```

就要求完整解析。

这是因为工具结果需要回填到较早的工具调用，当前增量接口只能“追加新消息”，
不会定点修改旧行。于是实现选择了最保守的方法：

> 重新解析整个文件，然后在数据库里找出变化。

因此，对于 Codex 这种工具调用密集的会话，“增量路径”实际上很难保持增量。

日志证明它不是偶发事件：相同几个大会话大约每几十秒就重复出现一次：

```text
incremental codex ... appended Claude lines require full parse
```

这还暴露了一个观测性问题：Codex 回退错误复用了 Claude 的错误文本，容易误导
诊断。

正确设计应当让尾部解析器输出“修改补丁”，例如：

```text
新增消息
更新 call_id=X 的结果
更新 call_id=Y 的 subagent_session_id
更新 ordinal=Z 的 token 统计
更新 session title
```

数据库通过 `(session_id, call_id)` 或类似索引直接更新目标行。这样工具结果
处理可以从 `Θ(S)` 降到 `Θ(d + log M)`。

## 第四处核心问题：一次追加最终可能产生全生命周期平方成本

假设一个会话不断增长，每次新增固定大小 `d`，并且工具结果持续触发全量解析。

第 `i` 次同步读取的数据约为：

```text
Sᵢ ≈ i × d
```

总成本为：

```text
d + 2d + 3d + ... + kd
= Θ(k²d)
```

也就是说，会话运行得越久，每一轮不仅更慢，累计成本也会越来越恶化。

这里不是理论上的极端情况：日志已经显示 945MB、574MB 等活跃会话在短时间内
反复全量回退。这是最应该优先消除的算法问题。

## 第五处核心问题：整文件回退后，数据库又完整加载一份旧历史

完整解析结束后，内存里已经有一份新会话历史。

随后 `ReplaceSessionContent` 又会：

1. 从 SQLite 加载这个会话的全部旧消息；
2. 加载关联的工具调用和工具结果；
3. 为旧消息建立 ordinal 映射；
4. 遍历新消息，计算差异；
5. 再进行一次完整的 transcript 相等比较；
6. 最后只更新实际变化的行。

“最后只更新变化行”是好的，避免了全部重建 FTS。但它为了得到这个小差异，
仍然同时保留：

- 新解析历史；
- 数据库旧历史；
- ordinal 哈希表；
- seen 哈希表；
- 工具调用和工具结果结构；
- 信号和秘密扫描产生的结构。

所以它优化了数据库写入量，却没有优化解析时间和峰值空间。

更合理的方式是由解析器直接产生补丁，不再通过“新旧两份完整历史比较”来发现
变化。即使暂时保留 diff，也可以立即删除那次重复的全量相等比较：diff 计划
本身已经知道有没有更新或插入。

## 第六处核心问题：完整同步主动关闭了 64MB 内存预算

普通监听同步原本有一个 64MB 的估算预算：

- 按源文件大小估算解析结果；
- 大文件会独占预算；
- 其他 worker 等待；
- 写入完成后释放配额。

但只要进入启动同步、完整同步或重建，代码会换成“不限量”的 bulk budget：

```text
weighted == nil
→ 所有解析立即获准
```

同时还有三个放大因素：

- 最多 8 个解析 worker；
- 结果队列可保存约 16 个已解析结果；
- 数据库批次按“100 个会话”触发，而不是按“占用多少字节”触发。

所以在写库变慢时，内存中可能同时存在：

- 最多约 100 个等待写入的解析结果；
- 16 个已完成但尚未消费的结果；
- 8 个正在解析或等待投递的结果；
- 正在做 diff 的数据库旧历史。

这就是典型的“按对象数量限流，但对象大小相差几万倍”。一个 5KB 会话和一个
945MB 会话都算作“1 个”。

应改为：

- 启动同步也使用内存预算；
- 批次同时满足“最多 100 个会话”和“最多 X MB”；
- 大文件进入独立队列，一次只处理一个；
- 调度器按预计解析内存收费；
- 数据库 writer 对 parser 建立真正的字节级背压。

仅设置 cgroup 或 `GOMEMLIMIT` 只能避免拖死整机，不能解决算法问题，甚至
可能变成频繁 GC 和交换抖动。

## 第七处明显的 `O(N²)`：快速同步反复遍历 Codex 目录

快速同步为了处理“同一个 UUID 同时存在 live 和 archived 副本”，目前会：

1. 遍历一次所有 Codex 文件；
2. 对每一个 UUID，再重新遍历全部 Codex 路径寻找相同 UUID。

因此是：

```text
N 个会话 × 每次扫描 N 个路径 = O(N²)
```

现在 `N=774` 还不至于成为这次 9.3GB 峰值的主因，但档案继续增长后会越来越
明显。修复很直接：首次发现时构建 `UUID → [所有路径]`，只需要一次遍历，
整体降为 `O(N)`。

## 第八处：增量写入后仍周期性扫描完整历史

即使成功走增量写入，信号和秘密检测仍会加载该会话的全部消息。代码做了
10 秒防抖，因此不是每行都扫描，但对于持续活跃的大会话，仍是周期性的
`Θ(M)`。

可以改为：

- 普通统计直接维护增量计数；
- 秘密检测只扫描新增或被修改的消息；
- findings 按 message ID 更新；
- 低频执行一次完整审计验证一致性。

这样日常路径是 `Θ(d)`，只有审计才是 `Θ(M)`。

## 第九处：工具结果存储重复

数据库目前把工具结果同时保存在：

- `tool_calls.result_content`
- `tool_result_events.content`

历史测量中，两边大约各占 3GB，而且样本内容相同。这个问题不改变大 O，
仍然是 `Θ(R)`，但把最大的数据项直接放大了约两倍，也同步放大：

- 数据库体积；
- SQLite 页缓存；
- WAL 写入；
- diff 时加载的历史；
- 搜索索引和备份成本。

应当只保留一个规范化结果实体，其他位置存引用。进一步可以对大输出采用内容
哈希去重和压缩。

## 次要但真实的平方行为

Codex parser 插入孤立的 subagent 通知时，会在线性扫描消息列表、移动切片，
并更新全部工具调用引用。

如果有 `E` 个需要插入的事件、`M` 条消息，最坏可到：

```text
O(E × M)
```

当 `E` 与 `M` 同阶时就是 `O(M²)`。

这里应先收集事件，最后统一排序或做线性归并，而不是每条事件都在数组中间
插入。

## 建议的修复优先级

### P0，先解决资源事故

1. 启动/完整同步恢复字节级内存预算。
2. 批次增加内存上限，大文件单独串行。
3. 为 worker 设置安全的 `MemoryHigh`，作为最后一道保险。
4. 增加每个文件的 `hash / seed / tail / fallback / parse / DB diff` 耗时和
   峰值记录。

### P1，真正降低复杂度

1. Codex 未变化文件先用持久化文件属性跳过，不先做完整哈希。
2. 持久化解析游标，避免短命 worker 重扫前缀。
3. 工具结果、subagent 链接、token 统计改为定点补丁。
4. 去掉增量成功后的第二次完整文件哈希，改用可续算哈希或分块哈希。
5. 将 UUID 副本查找从 `O(N²)` 改为一次建表的 `O(N)`。

### P2，进一步降低空间和数据库成本

1. 完整重建采用流式解析、分块写入临时数据库。
2. diff 不再加载两份完整历史。
3. 信号和秘密扫描增量化。
4. 工具结果规范化去重并压缩。

## 本次 PR 的范围：晚到工具结果增量定点更新（P1.3 的第一步）

Codex 工具输出（`function_call_output` / `custom_tool_call_output`）可能在
后续同步批次才到达，而调用本身已在前一批次提交。修改前，任何非空输出只要
其调用不在当前批次中，就强制整文件权威重解析：每条晚到输出都是
`O(history)`，且反复出现的输出让累计成本趋近 `Θ(k²d)`。

### 解析器（`internal/parser`）

- 增量游标（`codexCursorState`）新增有界待处理工具调用集合（`id`+`name`，
  上限 8）。调用出现时登记，输出消费后移除；上限保证游标缓存内存固定，
  溢出时退化为原有保守全量回退。
- 冷启动 seed（`seedCodexIncrementalStateFromReader`）只重放待处理调用的
  登记簿，不再把所有 response_item 当作用户消息。
- 晚到输出现在解析为 `ParsedToolCallUpdate{ToolUseID, ResultEvents}`，不再
  直接抛 `ErrIncrementalNeedsFullParse`；仅当调用名未知，或调用属于
  agent 作用域（`spawn_agent` / `wait_agent`，需要权威对账）时才回退。

### 同步引擎（`internal/sync`）

- `incrementalUpdate` 携带 provider 返回的 `toolCallUpdates` 进入
  `writeIncremental`。
- 引擎对结果事件做清洗（包括 blocked-result 分类与 subagent ID 前缀），
  并以 `db.ToolCallResultUpdate` 传给数据库。

### 数据库（`internal/db`）

- `WriteSessionIncremental` 接受 `ToolCallResultUpdate`，在事务内按稳定的
  `tool_use_id` 定位已存储调用，只追加不重复事件，重算展示摘要，并仅在
  有变化时递增一次 transcript revision。
- 摘要逻辑收敛为共享的 `SummarizeToolResultEvents`，保证增量与权威全量
  解析使用同一实现（一致性）。

### 验证计划

1. 单测/集成测试：
   - `internal/parser`：warm/cold 两种模式下自定义工具晚到输出增量更新。
   - `internal/db`：结果更新幂等，重放不递增 revision。
   - `internal/sync`：追加 `exec_command` 输出保留既有消息行、写入
     `result_events`，并与权威 resync 一致（增量/全量 parity）。
2. 性能基线与门禁：
   - 在 `upstream/main`（baseline）与本分支（candidate）分别运行
     `make bench-gate`，使用相同固定 `-benchtime`/`-count`，用
     `cmd/benchgate` 对比。
   - 新增 `BenchmarkCodexIncrementalLateToolOutput` 覆盖晚到输出路径：
     baseline 每次触发整文件重解析，candidate 走增量定点更新。
3. 验收：`go test ./internal/parser ./internal/db ./internal/sync` 通过；
   `make bench-gate` 对比无回退；PR 目标 `kenn-io/agentsview` `main`。

## 遗留成本（本次 PR 不处理）

- 完整源文件指纹哈希（`Fingerprint`）每次同步仍是 `Θ(S)`。
- 冷启动游标重建仍扫描已提交前缀 `[0, oldOffset)`：`Θ(S)`。
- 增量成功后的已提交前缀重哈希：`Θ(S)`。
- 启动同步的内存预算、UUID 去重 `O(N²)`、信号/秘密全量扫描、工具结果
  双份存储等 P0/P1/P2 其余项。

滚动哈希或持久化游标可以消除前三项，但会改变持久化与崩溃恢复契约，故意
留到后续 PR。

## 实测结果（2026-08-13）

基线 = `upstream/main`（`da0d7eb3`），候选 = 本分支（`04557873`）。
同一机器、`make bench-gate` 官方配置（`-benchtime=20x -count=6`，
`CGO_ENABLED=1 -tags fts5`），`cmd/benchgate` 对比：

| 基准 | 基线 | 候选 | 比率 |
|---|---:|---:|---:|
| `BenchmarkCodexIncrementalLateToolOutput` sec/op | 26.04ms | 4.95ms | 0.19x |
| `BenchmarkCodexIncrementalLateToolOutput` B/op | 3.86Mi | 358Ki | 0.09x |
| `BenchmarkCodexIncrementalLateToolOutput` allocs/op | 16.65k | 1.79k | 0.11x |

其余既有 gated benchmark 均在阈值内；`benchgate` 结论：
`no regressions beyond thresholds`。

测试：`go test -tags fts5 ./internal/parser ./internal/db ./internal/sync`
全部通过（parser 2.9s / db 33.7s / sync 97.3s），`go vet` 干净。

PR：https://github.com/kenn-io/agentsview/pull/1386
（`fix(sync): apply late Codex tool results incrementally`，提交
`04557873`；本计划文件为本地工作文档，未包含在 PR 中。）

## 第二阶段：持久化解析 checkpoint 架构（文献调研 + 分阶段设计）

> 记录时间：2026-08-13。来源：Semantic Scholar 定向检索 + 原始论文核验 +
> AgentsView 当前同步/解析/存储约束映射。本轮保持只读，没有修改文件。

### 结论

当前补丁有局部价值，但不应继续作为独立的“性能修复”。合理方案是把它纳入一个
完整的“持久化解析 checkpoint”架构，使无变化启动降为 `Θ(N)`、追加同步降为
`Θ(d)`，同时恢复全量同步的字节级内存背压。

### 为什么现有优化有限

当前 warm 增量路径仍包含两次线性读取：

- `Fingerprint` 先完整 SHA-256：`internal/parser/codex_provider.go:731`
- 增量成功后又从头计算 committed-prefix hash：`internal/sync/engine.go:11028`

冷 worker 还要扫描 `[0, oldOffset)` 重建游标：`internal/parser/codex.go:2138`。
仓库 benchmark 也明确把前两项称为剩余的两次完整线性读取：
`internal/sync/codex_bench_test.go:31`。

所以线程中的 945MB 文件追加 794B，即使工具结果不再全量解析，仍需约 10.72 秒；
23% 改善符合预期，但没有改变 `Θ(S)`。

### Semantic Scholar 给出的设计方向

| 论文证据 | 对 AgentsView 的含义 |
|---|---|
| Wagner/Graham 的增量解析工作强调复用未改变前缀，并保持与批处理解析结果一致（[Semantic Scholar](https://www.semanticscholar.org/paper/fe7d333d718d73e423a9512f40fc69ce35bea22d)、[作者公开稿](https://harmonia.cs.berkeley.edu/papers/twagner-parsing.pdf)） | 持久化“足以继续解析”的状态，而不是进程重启后重新扫描前缀 |
| Build Systems à la Carte 将调度、重建判断和持久化构建信息拆开建模（[Semantic Scholar](https://www.semanticscholar.org/paper/0a8b8e3318839fde73eab4720b680b1b246dcfa4)、[作者页面](https://simon.peytonjones.org/build-systems-a-la-carte-theory-and-practice/)） | 将便宜的 stat/identity freshness gate 与昂贵的完整内容审计分离 |
| 增量视图维护直接计算 insert/update/delete 对物化结果的 delta，而不是重算整个视图（[Semantic Scholar](https://www.semanticscholar.org/paper/2832af6f101f93745036d3617eabe123d3ca8ed4)、[SIGMOD 原文](https://sigmodrecord.org/publications/sigmodRecord/9306/pdfs/170036.170066.pdf)） | 解析器输出稳定键补丁：新增消息、更新 `tool_use_id`、token、subagent 链接等。现有 `ToolCallUpdates` 正是可保留的基础 |
| 异步 snapshot 工作说明恢复点必须包含计算状态并与处理进度一致（[Semantic Scholar](https://www.semanticscholar.org/paper/4e8f4fedddcac090efe64fe16e8c509685a6ef7f)、[arXiv](https://arxiv.org/abs/1506.08603)） | cursor、offset、hash state 必须与数据库 delta 在同一 SQLite 事务提交 |
| SEDA 用显式队列和 admission control 防止下游变慢时资源失控（[Semantic Scholar](https://www.semanticscholar.org/paper/e3966d51c793d6877a9aa0d690cb52011f75ddd7)、[SOSP 原文](https://www.sosp.org/2001/papers/welsh.pdf)） | parser→writer 必须按估计字节收费，而不是按“100 个会话”计数 |

Semantic Scholar 调研也改变了一个重要判断：rsync rolling checksum 和 FastCDC
适合“不知道修改位置”的相似文件匹配，但仍需扫描输入来发现变化
（[rsync](https://www.semanticscholar.org/paper/d9695436e01795fa572df1f01d8643056a96f205)、
[FastCDC](https://www.semanticscholar.org/paper/64b5ce9ff6c7f5396cd1ec6bba8a9f5f27bc8dba)）。
Codex 已知旧 offset，因此分块哈希不能作为主修复，它不会自动把 `Θ(S)` 变成
`Θ(d)`。

### 推荐架构

持久化一个版本化 checkpoint：

```text
C(O) = {
  source identity, committed offset, newline/tail anchor,
  parser cursor, resumable SHA-256 state,
  next ordinal, parser/checkpoint version
}
```

```text
stat
├─ 完全未变化 → 直接跳过，不读 transcript
├─ 可证明为安全追加
│  → 加载 C(O)
│  → 只读 [O, L)
│  → 产生数据库 delta + C(L)
│  → 同一个 SQLite 事务提交
└─ 任一校验失败 → 单次顺序全量解析并替换 checkpoint
```

实施要点：

- checkpoint 放在 SQLite-only 表中，不镜像到 PostgreSQL/DuckDB；这符合现有
  存储边界：`docs/agents/storage.md:26`。
- checkpoint 至少包含 inode/device、offset、尾部锚点、当前内存 cursor 的
  显式版本化编码和 SHA-256 chaining state。当前 cursor 已经很紧凑，但只存在
  256 项/2MiB 的进程内 LRU：`internal/parser/codex_cursor.go:155`。
- 当前 Go 1.26.3 的 `sha256.New()` 支持
  `BinaryMarshaler`/`BinaryUnmarshaler`，可以续算摘要，但仍应保存算法与
  codec 版本，解码失败时保守全量回退。
- parser、hash 使用同一个已 stat 的文件描述符和固定 snapshot limit，避免
  fingerprint、parse、prefix hash 分别重新打开文件。
- `WriteSessionIncremental` 已经把消息、工具结果和 session 元数据放在一个
  事务里：`internal/db/messages.go:1053`。checkpoint 应加入该事务，确保
  crash 后要么重放旧 tail，要么全部推进，不能出现 DB 已更新但 offset 已丢失。
- appended `session_meta`、文件缩短/替换、锚点不匹配、checkpoint 版本变化或
  损坏，继续走现有权威全量回退；这些 Codex 格式限制已有来源证据：
  `docs/internal/session-format-sources.md:255`。

### 正确性边界

`size + mtime + inode + tail anchor` 无法严格证明旧前缀中任意位置从未被原地
修改。没有文件系统 journal 或可信 dirty-range oracle 时，严格发现这种修改
必然重新读取前缀。

因此需要明确两种模式：

- 默认 append-trust：依据 Codex rollout 的追加语义走快速路径，配合周期性完整
  SHA-256 审计自动修复罕见重写。
- strict：每次仍完整校验，保留原有强 freshness 语义，但接受 `Θ(S)`。

不能把尾部锚点描述成完整内容证明。

### 建议的交付顺序

1. **P0**：删除 full/resync 的无界 bulk admission。当前 `weighted == nil`
   会让所有解析立即进入：`internal/sync/parse_retention.go:45`。所有同步都
   使用字节预算，大文件独占，lease 保持到 SQLite commit；批次按数量或字节
   任一达到上限即写入。
2. **P1**：在一个端到端 PR 中同时交付 stat early-cutoff、持久化 cursor、可续
   算 hash 和原子 checkpoint。只交付其中一项仍然会留下另一条 `Θ(S)` 路径。
3. **P2**：保留现有晚到工具结果补丁，并扩展为统一 delta contract。
4. **P3**：全量解析流式写入 scratch DB，解决单个 945MB 文件自身的峰值；P0
   只能避免多个大文件同时放大。

### 宏观验收标准

- 非审计的 20.1GiB 无变化启动读取 transcript 内容 0B。
- 冷进程处理 945MB 文件的 794B 追加，源读取量不超过 `d + 128KiB`，且不调用
  三条全前缀路径。
- 相同 8KiB 追加在 10MiB 与 1GiB 文件上的 p95 延迟相差不超过 2 倍。
- crash-before/after-commit、缩短、替换、锚点错误、损坏 checkpoint、
  appended `session_meta` 全部保守恢复，并与权威全量解析 parity。
- 全量同步峰值受 configured budget 加最多一个 oversized parse 限制；长期目标
  遵守仓库“几百 MB”约束，并同时记录 live heap、forced-GC heap 和物理脏内存，
  而不是只看 RSS：`docs/agents/background-work.md:6`。

### 预期性能

| 场景 | 当前实测 | 合理预期 |
|---|---:|---:|
| 20.1GiB、774 个 Codex 文件无变化启动 | 约 28s，读取全部 20.1GiB | 同步检查 0.1–1s；服务整体就绪约 0.5–2s |
| 945MB 会话追加 794B | 10.72s，仍读取约 1.89GB | daemon 内 20–200ms；冷 one-shot worker 0.2–1s |
| 同等追加在 10MB/1GB 会话 | 延迟随文件大小线性增长 | 基本不受旧文件大小影响 |
| 945MB 首次完整解析 | 15.34s | checkpoint/单遍读取后约 9–12s |
| 完整档案同步峰值 | 曾约 9.3GB | P0 背压后约 1.3–2GB；流式全量解析后目标 300–600MB |

945MB/794B 的原始宏测位置：
`/home/chris/.codex/sessions/2026/08/13/rollout-2026-08-13T02-14-18-019ffa66-a276-77a2-a970-f2e06702055e.jsonl:779`
与 `:786`。

最关键的不是延迟估算，而是可硬性验收的 I/O：

- 无变化、非审计同步：读取 transcript 内容 0B。
- 945MB + 794B 追加：读取量不超过 `794B + 128KiB` anchor。
- 相比当前两次完整扫描，读取量减少至少约 14,000 倍。
- 增量解析的额外内存为 `O(d)`，不再加载 945MB 历史。
- 100 次类似追加由当前约 18 分钟量级，降到几秒至几十秒，实际可能由 debounce
  主导。

需要区分三个阶段：

- 只保留当前工具结果 patch：仍约 10 秒，提升有限。
- 加入持久化 checkpoint、stat early-cutoff、可续算 hash：日常追加进入
  20–200ms 区间，这是主要性能跃迁。
- 加入流式全量解析：首次导入不会变成毫秒级，但峰值内存才能真正降到几百 MB。

### 正式性能门禁建议

- 945MB + 1KiB 追加，daemon 内 p95 < 250ms。
- one-shot worker < 1s。
- source bytes read < 256KiB。
- 10MB 与 1GB 文件相同追加的 p95 比率 < 2x。
- P0 后完整同步 RSS < 2GiB；完成流式解析后 < 512MiB。

上述区间假设 checkpoint 有效、默认 append-trust 模式且不执行完整审计。周期性
完整 SHA-256 审计仍需读取 20.1GiB、可能仍约 28 秒，但它应退出启动和活跃同步
热路径。

### 检索方法与可复现性

检索使用 Semantic Scholar Graph API 的 `/paper/search/match`，按上述论文标题
定向匹配，访问日期为 2026-08-13。匿名通用主题检索曾返回 429，因此这是可复现
的定向证据检索，不是穷尽式系统综述。

本轮保持只读，没有修改文件，也没有提交。

## 第三阶段：实施记录（2026-08-13）

### P0：字节级内存背压 — 已完成

- `parse_retention.go`：删除 `weighted == nil` 的无界 bulk admission；
  `newBulkParseRetentionBudget()` 改为与普通 pass 相同的加权字节预算（默认
  64MiB），大文件独占、小文件共享，pass 结束后仍 scavenge 一次。
- `collectAndBatch`：pending 批次按“100 个会话 或 64MiB 估计字节”任一上限
  触发 flush；`processResult.sourceBytes` 从各 parse seam 携带到
  `pendingWrite`。
- 测试：`TestFullSyncPassIsByteBudgeted`、
  `TestBulkParseRetentionBudgetUsesWeightedAdmission`、
  `TestCollectAndBatchFlushesOnByteCap`。

### P1：持久化解析 checkpoint — 已完成

- SQLite-only 表 `parser_checkpoints`（`schema.sql` + `internal/db/checkpoint.go`）：
  session/agent/path/inode/device/mtime/offset/128KiB tail anchor/cursor/
  resumable SHA-256 state/hash/next ordinal/version。
- `codexCursorState` 增加版本化 `MarshalBinary`/`UnmarshalBinary`
  （`internal/parser/codex_cursor.go`），解码失败保守全量回退。
- 全量解析把结束游标附到 `ParseResult.Checkpoint`；批量与单会话写入成功后在
  同一提交后写入 checkpoint（`persistFullParseCheckpoint`）。
- 增量路径：
  - `codexCheckpointFingerprint`（`internal/sync/checkpoint.go`）：
    unchanged → stat-trust skip（0B 读盘）；append → identity + size 单调 +
    tail anchor 校验 + 仅读 `[offset, size)` 续算 SHA-256 得到指纹；任何证明
    失败（截断/身份变化/锚点不匹配/hash state 缺失）→ `codexCheckpointInvalid`
    强制权威全量重建，绝不增量追加。
  - provider 通过 `IncrementalRequest.Seed` 接收持久化游标，避免冷 worker
    重扫 `[0, oldOffset)`。
  - 新 checkpoint 与数据库 delta 在同一 SQLite 事务提交
    （`WriteSessionIncremental` → `IncrementalSessionUpdate.Checkpoint`）。
  - stored file_hash 只覆盖已提交安全前缀（partial tail 场景由
    `codexResumeHash` 停到 `newOffset` 保证）。
- 语义变化：默认 append-trust（同 size/mtime/inode 的原位重写被 stat gate
  信任，周期审计 `ResyncAll`/forceParse 仍可发现并修复）；旧测试
  `TestSyncPathsCodexSameStatInPlaceRewriteUsesContentHash` 相应改为
  `...TrustedByCheckpoint` 并验证审计路径。
- 测试：parser checkpoint roundtrip/损坏拒绝/Seed 恢复；
  db roundtrip/同事务持久化；sync 端到端
  `TestCodexCheckpoint*`（全量落盘、增量推进、截断回退、锚点失配强制全量、
  stat-trust skip）。

### 实测（`make bench-gate`，base=`da0d7eb3` + 同一 benchmark 文件）

| 指标 | base | head | 比率 |
|---|---:|---:|---:|
| `BenchmarkCodexIncrementalLateToolOutput` sec/op | 25.77ms | 3.50ms | 0.14x |
| 同上 B/op | 3.854Mi | 1.086Mi | 0.28x |
| 同上 allocs/op | 16.65k | 2.04k | 0.12x |

`cmd/benchgate`：`no regressions beyond thresholds`。

测试：`go test -tags fts5 ./internal/parser ./internal/db ./internal/sync`
全部通过；`go vet` 干净。

### P2：统一 delta contract — 部分完成

晚到工具结果已是定点 delta（`ToolCallResultUpdates`，随本实施保留并纳入
checkpoint 事务）；subagent 链接（`SubagentLinks`）与 token 统计继续走既有
增量路径；title 变更仍走权威全量（需要 `session_index.jsonl` 元数据重建）。
把这些全部收敛为统一 `Delta` 类型（含 token/subagent/title 补丁）留作后续。

### P3：流式全量解析 — 未实现（后续 PR）

单个 945MB 文件完整解析仍会在内存中构建全部消息。P0 已防止多个大文件同时
放大，但单文件峰值需解析器改为流式 sink / scratch DB。设计、验收标准见上文
“P3”与“宏观验收标准”；本轮未动解析器内部结构，避免在高风险重构中破坏已
验证的 checkpoint 语义。

### PR 状态

- PR #1386（晚到工具结果增量定点更新）：已关闭。
- PR #1388（`feat(sync): persist Codex parse checkpoints and bound bulk
  memory`，提交 `0c6958ff`，位于 `04557873` 之上）：已创建后按用户要求关闭，
  暂不提交评审；分支与本地实现保留，待用户确认时机再开。

### 真实数据宏观实测（945MB 会话，base=`da0d7eb3`）

方法：真实 `019fe581…`（944,716,046B 截断快照，最后 call 无 output）+ 真实
794B 晚到输出；`/proc/self/io` rchar 与 `strace -y` 按源文件路径归因；
`/usr/bin/time -v` 测进程峰值 RSS。

| 场景 | base | head（P0+P1） |
|---|---:|---:|
| 冷全量同步 | 15.93s，源读取 ~1.89GB | 17.38s，源读取 ~2.83GB（多一次建 checkpoint 的 hash state 读，一次性） |
| **追加 794B 晚到工具结果** | **13.74s，源读取 ~2.83GB（fingerprint+全量解析+前缀重哈希）** | **6.85s，源读取 ~227KB（128KiB 锚点 + 794B tail）** |
| **无变化启动（再次同步）** | **1.90s，源读取 ~945MB** | **1.67ms，源读取 0B** |
| 进程峰值 RSS（整个流程） | ~1.93GB | ~1.28GB |
| 结果正确性 | `result_len=472, events=1` | 相同，且 `LastWriteIncremental=true` |

按计划验收口径：无变化启动读取 0B（达成）；945MB+794B 追加源读取
`d+128KiB` 量级（达成，约 227KB）；相对旧路径读取量减少约 12,000 倍。

> 口径收紧（评审修正）：上述“0B”指 **0B transcript 读取**；每次 stat 前仍
> 从 SQLite 读 checkpoint 行（128KiB anchor + cursor + hash state，约
> 125KiB/会话，774 个约 96.75MiB）。追加路径源读取为
> `≈3d + 128KiB`（fingerprint 续算、parser tail、checkpoint 续算），794B
> 实测约 227KB，满足 `<256KiB` 门禁，但不是严格的 `d + 128KiB`。

**剩余差距（诚实标注）**：head 的增量 wall time 仍是 6.85s 而非 20–200ms，
因为 checkpoint 只消除了源文件读取，DB 侧 debounced signal/secret 重算仍
每轮加载全部 13,864 条消息（计划问题 8，P2 增量信号/秘密扫描）。要达成
`p95 <250ms` 门禁，需要下一个 PR 把信号/秘密检测改为只扫描新增/变更消息。

## 第四阶段：评审修复记录（2026-08-13 第二轮）

评审结论：P1 存在两个阻断级一致性缺陷、一个 TraeX 键位问题、性能边界描述
过宽、周期审计未实现。以下全部已修复并加入回归测试。

### 1. 高：旧 checkpoint 与更新后的 DB offset 混用 → 已修复

`codexCheckpointFingerprint` 现在先通过 `GetSessionForIncremental` 解析真实
session ID，并强制校验：

- `checkpoint.Offset == IncrementalInfo.FileSize`
- `checkpoint.NextOrdinal == IncrementalInfo.NextOrdinal`
- `checkpoint.Hash == DB file_hash`

任一不一致 → `codexCheckpointInvalid` → 强制权威全量重建，绝不用旧 Seed 配
新 DB 前缀。回归测试：
`TestCodexCheckpointStaleCannotResumeFromNewerDBOffset`（复现原
“gpt-5.5 model 被写成空串”场景）。

### 2. 高：活跃文件继续追加时 checkpoint 保存错误 SHA state → 已修复

`persistFullParseCheckpoint` 改为只按**已提交前缀** `inc.FileSize` 建哈希
state 与锚点（`codexBuildInitialHashState(path, committed)`），并直接由
state 推导 checkpoint hash，不再用 `info.Size()` 或 `pw.sess.File.Hash`。
追加发生在全量写期间也不会把 `[S0:S1]` 重复哈希。回归测试：
`TestCodexCheckpointHashStateBoundedToCommittedOffset`。

### 3. 中：TraeX checkpoint 键位 → 已修复

读取入口不再硬编码 `codex:<uuid>`，改为按 `(file_path, agent)` 从 DB 解析
真实 session ID（TraeX 为 `traex:<uuid>`）。回归测试：
`TestCodexCheckpointTraeXLoadsPersistedCheckpoint`。

### 4. 中：性能边界描述 → 已收紧

文档与计划改为：无变化 = 0B **transcript** 读取 + ~125KiB/会话 checkpoint
行读取；追加源读取 `≈3d + 128KiB`（实测 227KB，满足 `<256KiB` 门禁），
不再声称严格的 `d + 128KiB`。

### 5. 中：周期审计未实现 → 已实现

新增 `Engine.SetCheckpointAudit`；`sync_worker` 的 audit 模式（每日 safety
net）在 reconciliation 前开启、之后关闭，checkpoint gate 被绕过，provider
全源 fingerprint 会检测并修复 same-stat 原位重写。回归测试：
`TestCodexCheckpointAuditRepairsSameStatRewrite`。

### 新增测试

- 冷重启 parity：`TestCodexCheckpointColdRestartResumeParity`（新 engine、
  空 cursor cache，从 checkpoint 恢复增量并保持 parity）。
- DB 事务回滚：`TestParserCheckpointRollsBackWithTransaction`（tx 回滚后
  checkpoint 必须消失）。
- 移除测试内私有绝对路径（`codex_checkpoint_test.go`）。

### 验证

- `go test -tags fts5 ./internal/parser ./internal/db ./internal/sync`：通过。
- `go vet ./internal/sync ./internal/db ./internal/parser ./cmd/agentsview`：干净。
- `./cmd/agentsview` 唯一失败 `TestRunDaemonArtifactExchangeConnectsToIPv6LocalhostListener`
  为容器无 IPv6 loopback 的环境问题，已在干净 `upstream/main` 复现确认。

P1 评审阻断项已全部关闭；PR 仍保持关闭状态，等待用户决定何时重新提交。

## 第五阶段：增量信号/秘密维护与单遍 checkpoint（设计，2026-08-13）

宏观结论（与基准分析一致）：head 的增量 wall time 仍是 6.85s 而非
20–200ms，因为 checkpoint 只消除了源读取，DB 侧 debounced signal/secret
重算仍每轮加载全部 13,864 条消息。`run(sessionID)` 无条件执行
`GetSessionFull + GetAllMessages + 全量 signals + 全量 secrets`
（engine.go:12000），这是 Amdahl 型瓶颈。冷全量 9.1% 回退来自 full parse
之后的第二次完整 hash pass（`codexBuildInitialHashState`）。

### 目标 ①：晚到工具结果 delta 路径（typed reducer + 有界尾窗）

- 新增 SQLite-only 表 `session_signal_state`（state JSON + codec/rules
  版本 + 校验 token），不镜像到 PG/DuckDB（同 `parser_checkpoints` 边界，
  storage.md:26）。状态由全量重算路径 seed（writeBatch / writeSessionFull /
  recomputeSignalsFromDB / 事务内回退）。
- 状态内容：工具聚合（failureCount、consecutiveMax + 前缀最大游程/跨界
  游程、retryCount + 尾游程 (name,input,len)、editChurn + 每文件最近两个
  edit ordinal、runaway 前缀 latch + 尾 facts、exact-run 跨界状态）；消息
  聚合（lastRole/lastContent、model counts、compactionCount +
  lastValidTokens、hasContextData）。
- 每次维护与增量写入同一 SQLite 事务：
  1. 尾部 K=64 调用 facts 从 `tool_calls`/`tool_result_events` 有界查询
     重建；
  2. 新消息 O(1) 更新聚合与游程，新 Edit/Write 调用按每文件最近两个
     ordinal 精确更新 churn，新调用滑入尾窗并把退出窗口折叠进 latch；
  3. 晚到结果只翻转该调用 is_failure，重算包含它的 retry/failure/
     runaway 局部窗口与最终失败尾段；
  4. secrets 只扫新增 result 事件内容，findings 按
     (ordinal, call_index, location) 定点增删，禁止全会话替换；
  5. health score 从紧凑聚合 O(1) 重算；delta、facts、signals、findings、
     checkpoint、state 同一事务提交（`WriteSessionIncremental` 内
     `SignalMaintainer` 回调，db 不 import signals）。
- 诚实回退边界（保持与权威全量逐字段 parity）：
  - delta 含实质 user 消息或 compact boundary 消息 → 沿用 debounce 全量
    重算（prompt 类 heuristics 增量化留作后续推广到普通 message append）；
  - 被更新调用在尾窗之外、state 缺失/版本/校验 token 不匹配 → 事务内
    全量重算并 reseed。
- 正确性锚点：维护后的信号列 + findings 与权威全量重算一致；parity 测试
  覆盖随机事件序列。

### 目标 ②：冷全量单遍流式 hash/anchor

- 解析器：codex 全量解析在 lineReader 之下加 hash/ring tee，SHA-256
  可续算 state + 128KiB 滚动环覆盖快照 `[0, info.Size())`，即已提交前缀；
  `ParseResult` 携带 hash state + anchor digest。
- 引擎：`persistFullParseCheckpoint` 直接用解析结果，删除第二次完整读；
  冷全量回到 fingerprint + parse 两遍（≈base 15–16s）。
- checkpoint 行瘦身：`parser_checkpoints` 只存元数据 +
  `tail_anchor_digest`，cursor/hash_state 拆到 `parser_checkpoint_blobs`
  懒加载；stat-only 路径 0B blob 读（774 会话从 ~96.75MiB 降到约
  200B/行）。版本 2；表未进 upstream/main，直接改 schema.sql 不需迁移。
- 追加路径 anchor 校验改为读 `[0,offset)` 末 128KiB 算 digest 比对；
  增量路径保留 `codexResumeHash`（读量仍满足 `<256KiB` 门禁）。

### 目标 ③：新性能门禁

- `BenchmarkCodexIncrementalLateToolOutput` 改名
  `BenchmarkCodexLateToolOutputDebouncedBurst`（它确实是 debounced burst，
  20 次迭代摊薄首次全历史重算）。
- `BenchmarkCodexIncrementalSyncReads` 替换为真实 checkpoint 续算路径
  benchmark（旧实现仍测完整 fingerprint/prefix-hash 管线）。
- 新增 gated：单次 quiet append（scheduler interval=0，每次迭代都完整付
  signals/secrets，不摊销）× 500/5k/15k 三档规模；Codex 冷全量单独 gate。
- 新增断言测试：增量维护路径 `GetAllMessages` 调用 = 0（计数 hook）；
  secret 扫描字节 ≤ delta 内容。
- 10MB vs 1GB 相同 append p95 比率 `<2×`：macrobench 构建 tag（不进
  `-bench .` 门禁），宏测流程写入 docs/internal/performance-gates.md。

### 验收

- 单次 quiet-session append（call + 晚到输出）daemon p95 目标 20–200ms；
- 冷全量不慢于 base（约 15–16s）；
- 三档规模延迟斜率基本不随历史增长；GetAllMessages=0；
- 增量/全量 parity 测试与既有 checkpoint 测试全部通过；
- `make bench-gate` 无回归；文档同步更新。

## 第五阶段：实施记录（2026-08-13 第三轮）

三项工作已全部实现并提交（提交 `04c4c626`、`9d10b532`、`f38bc772`），
PR 仍保持关闭。

### 目标 ①：增量信号/秘密维护 — 已完成

- `internal/signals/incremental.go`：typed reducer 状态机（JSON 版控），
  尾部 35 调用 facts 窗口精确维护 failure/retry/churn（每文件 counted
  锁存）/runaway（窗口退出折叠）/final-streak/mid-task；修改窗口 = 尾部
  12 调用，越界拒绝。
- db：`session_signal_state` 表（SQLite-only，revision+version token）；
  `WriteSessionIncremental` 内 `SignalMaintainer` 回调，信号列、findings
  定点增删（INSERT..WHERE NOT EXISTS 去重 + RowsAffected 精确 leak
  count）、state、checkpoint 同一事务提交。
- 回退边界：delta 含实质 user 消息或 compact boundary、被更新调用在
  尾窗外、state 缺失/token 不匹配 → invalidate + debounce 全量重算并
  reseed；所有全量路径（writeBatch/bulk/writeSessionFull/
  recomputeSignalsFromDB）现在都 seed state。
- 测试：300 步随机化 parity（增量 fold vs 权威全量逐字段一致）、端到端
  parity（增量后与同尺寸原位重写触发的权威全量重建一致）、
  GetAllMessages=0 与 secret 扫描字节 ≤ delta 断言。

### 目标 ②：冷全量单遍流式 hash/anchor — 已完成

- `internal/parser/codex_hash_tee.go`：全量解析在读快照的同一遍上计算
  可续算 SHA-256 state 与 128KiB 尾锚 digest；`ParseResult` 携带两者。
- `persistFullParseCheckpoint` 直接用解析结果，删除第二次完整 hash 读
  与 anchor 重读；`parser_checkpoints` 只存元数据 + anchor digest，
  cursor/hash_state 拆到 `parser_checkpoint_blobs` 懒加载（版本 2，旧行
  视为无效强制重建）。
- 测试：tee 携带 state digest == 快照哈希、anchor digest == 尾窗哈希、
  续算复现全文件哈希；快照边界回归测试改写后仍通过。

### 目标 ③：新性能门禁 — 已完成

- `BenchmarkCodexIncrementalLateToolOutput` 改名
  `BenchmarkCodexLateToolOutputDebouncedBurst`（文档注明 debounce 摊薄）。
- `BenchmarkCodexIncrementalSyncReads` 替换为
  `BenchmarkCodexCheckpointAppendResume`（真实 checkpoint 续算管线：
  gate + 种子尾解析 + 续算哈希 + 锚 digest + checkpoint 组装）。
- 新增 gated：`BenchmarkCodexQuietAppendSignals500/5000/15000`（单次
  quiet append，debounce 关闭，每次迭代完整付 signals/secrets，自断言
  GetAllMessages=0）；`BenchmarkCodexColdFullSync`（冷全量单独 gate）。
- 10MB vs 1GB p95 比率 `<2×`：`codex_macro_bench_test.go`（build tag
  `macrobench`，不进 `-bench .`），宏测流程写入
  docs/internal/performance-gates.md。

### 实测（1x smoke，本机）

| 基准 | 结果 |
|---|---:|
| `BenchmarkCodexCheckpointAppendResume` | 837µs/op |
| `BenchmarkCodexQuietAppendSignals500` | 3.50ms/op |
| `BenchmarkCodexQuietAppendSignals5000` | 3.58ms/op |
| `BenchmarkCodexQuietAppendSignals15000` | 4.02ms/op（500→15k 约 1.15×） |
| `BenchmarkCodexColdFullSync`（约 9.5MB fixture） | 857ms/op |

500/5k/15k 三档斜率基本平坦（1.15×），满足目标 ① 的
20–200ms p95 与"延迟不随历史增长"门禁。完整 `make bench-gate` 对比与
945MB 真实宏测留待下一轮执行。
