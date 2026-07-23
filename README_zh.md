# Docengine

[English](README.md) · [发展设计与路线图](develop.md) ·
[模块开发历程与设计决策](modules_develop.md)

Docengine 是一个实验阶段的 Go 本地 UTF-8 文档内核，目标是在不把完整文档载入
内存的情况下编辑大型文件。它从 TypeMD 后端抽离，现已成为独立模块：

```go
module github.com/moresleep512/docengine
```

## 仓库定位

Docengine 是本地文档编排内核，不是编辑器、渲染器或应用服务器。核心只理解字节、
UTF-8、范围、revision 和不可变 Snapshot；它不能理解 Markdown、JSON、代码语法或
任何其他文档格式。

当前已经提供：

- 磁盘支撑的持久化 Piece Tree 和不可变 Snapshot；
- 带 revision 检查的原子编辑批次及磁盘 undo/redo；
- 只以完整批次作为持久化编辑单位的 v2 追加式恢复 journal；
- 全文件 UTF-8 校验和 SHA-256 基础文件身份；
- 保证替换边界 UTF-8 安全的事务编辑与恢复回放；
- 流式、带强冲突检查的原子保存；
- POSIX 父目录同步，以及带有界瞬态错误重试的 Windows `ReplaceFileW`
  write-through 替换；
- 符号链接真实目标固定，以及提交后故障的显式只读状态；
- 绑定 revision、查询读取有界且带有界 LRU 窗口缓存的 byte/line/rune 坐标索引，并支持
  由 ChangeMap 驱动、可证明安全的 checkpoint 前缀/后缀复用；
- 编辑、undo、redo 返回顺序 ChangeMap，并支持带前后粘性的 Anchor；
- Session 托管有界 ChangeMap 历史、线性映射链组合、正反 revision 查询、带 lineage
  校验的索引刷新，以及可取消的原子批量 Anchor/范围变换和 opaque 泛型 annotation；
- 有界、可续接的 Session 事件流，精确报告慢消费者丢失量，发布保存进度与恢复 WAL
  耐久性跃迁，并提供并发关闭屏障；
- Session 资源限制、journal 同步周期以及 shared/owned 运行时目录策略均可配置并可查询；
- 宿主持有的 Snapshot、coordinate Index 与 virtual Pager 在所有新旧 generation 间共享
  同一个硬上限，并提供原子生命周期统计；
- Save/Commit/Undo/Redo 均有可取消版本，Close 支持超时停止等待且不会中止同一个后台
  资源清理屏障；
- 通过跨平台文件锁保护的 owned Session 崩溃孤儿回收；
- 不破坏 Snapshot 的首版 Piece/undo 压缩，以及通过显式保存检查点完成的 journal 重基；
- recovery journal 默认 4 GiB 硬上限、精确增长统计，以及显式启用且失败后退避的自动
  保存检查点；
- 绑定 revision、UTF-8 安全的逻辑 Page 虚拟化，带严格 Page/窗口预算、有界 LRU 缓存
  和并发任务背压；
- generation 原子发布的格式中立 Fragment、明确的索引水位、定点 Measure 索引、
  byte/Fragment/Measure 三类锚点、非对称 overscan 及巨型 Fragment continuation Page。

当前还没有全文搜索、多源 Composition、协作、远程存储、UI 或稳定的 1.0 API。
各项能力的格式中立边界见 [develop.md](develop.md)。

## 与 TypeMD 的关系

原实现位于 TypeMD 私有后端包中。抽离过程中删除了 Markdown 块扫描、格式专有元数据、
SQLite 搜索、索引发布、Wails 绑定和编辑器布局策略。

Docengine 与 TypeMD 不会自动同步；TypeMD 必须显式迁移到本模块后才能获得这里的更新。

## 架构

```text
未来宿主：桌面应用 / CLI / 服务 / 格式适配器
                         |
                         v
                   document.Session
          revision、事务、历史、保存
       /          |          |            |             \
      v           v          v             v              v
 document/store recovery document/save document/coordinate document/virtual
  Piece Tree   v2 WAL      原子替换       索引/ChangeMap    Page/Fragment
       \          |          /             /              /
        +---------+---------+-------------+--------------+
                           |
                           v
                 操作系统文件与 io.ReaderAt
```

`document/store` 是最底层。它用指向外部 `io.ReaderAt` 字节范围的 Piece 表示逻辑
正文；持久化随机 Treap 提供结构共享、平均对数编辑、有界读取和不可变 root。零值维护
策略会在 4,096 个 Piece 时自动合并同 Source 连续范围；无收益扫描会把下次触发点后移
一个阈值。`store.Options` 可修改或关闭该策略，`Tree.Stats` 无需读取正文即可报告下次
触发点和已完成的自动压缩次数。

`recovery` 把每个逻辑事务存为一个带校验的 v2 batch。96 字节 `DOCLOG02` 文件头
用规范化真实路径和完整基础内容 SHA-256 绑定 journal；只有 `DOCJNL02` 批次的头、
操作表、payload 和 CRC-32C 全部合法后才会参与回放。损坏尾部可以修复且绝不会暴露
半个事务。
`Journal.Size`、`BatchEncodedSize` 和 `BatchAppendResult.EndOffset` 无需解码或分配
候选 batch，就能报告精确物理增长。

`document/save` 把 Snapshot 流式写入同目录临时文件，同步后做最后一次完整内容冲突
检查，再原子替换目标。POSIX 替换已经成功但父目录同步失败时，会返回类型化
`DurabilityError`，调用方不会把“已提交但耐久性未知”误当成“没有替换”。

保存期间若有更新编辑到达，Session 会在替换基础文件之前，为新基础预建并同步 journal，
同时同步其父目录项；最后身份检查、journal 准备和替换共享一个很短的写屏障。进程退出后
因此只会留下“旧基础+旧 journal”或“新基础+已耐久的新 journal”。即使没有并发编辑，
保存也会为这个崩溃窗口准备一个空的新 journal。重开时必须恰好有一个候选与当前完整
基础 fingerprint 匹配；已退役候选会被隔离，零个或多个匹配仍然阻止打开。

`document.Session` 负责 Piece Tree、revision 历史、恢复、Source generation、保存
重基和生命周期。`OpenContext` 单次流式扫描完整文件，同时校验 UTF-8、计算 SHA-256
并统计换行；扫描前后还会比较操作系统变更代际（POSIX `ctime` 或 Windows
`ChangeTime`），因此无需二次读取即可拒绝恢复了 mtime 的同长度原地改写。Metadata
同时报告请求路径和真实路径，保存始终固定到真实目标。若原子
替换后重绑定失败，Session 会保留读取能力并永久禁止继续修改。

`OpenOptions` 中的零值限制会解析为明确默认值：每批 256 个操作、单次插入 1 MiB、
undo 存储 256 MiB、保留 256 个事件和 256 个 ChangeMap、每批最多 65,536 个 Anchor、
journal 默认硬上限 4 GiB，并且每秒同步一次。自动 journal checkpoint 默认关闭；
只有显式设置 `AutoCheckpointJournalBytes` 才授权后台保存，失败后下一触发点会后移
一个完整阈值。`Session.RecoveryStats` 原子报告物理字节、当前/下次阈值、排队状态和
已完成自动 checkpoint 数。宿主持有的 Snapshot、coordinate Index 和 virtual Pager 在
当前及已退役 generation 间共享默认 1,024 个 lease 的硬上限；`Session.LifecycleStats`
报告 active/peak lease、等待/执行中的保存、自动任务以及 closing/closed 状态。显式目录
默认是 shared；省略 SessionDir 时会创建唯一的 owned
目录。undo 文件使用无冲突的临时名称并在关闭时删除。owned 目录只有在确认为真实空
目录时才删除，脏恢复 journal 和宿主的未知文件始终保留。owned Session 目录包含持锁
的 v1 marker；自动清理和公开的 `ReclaimStaleSessionDirectories` 只删除未持锁、标记
合法且全部内容都可识别的 Docengine 遗留物，绝不递归删除未知内容。`Session.Config`
可读取最终解析后的策略。

`Session.Subscribe` 会把保留历史与实时事件原子衔接。每个订阅者使用有界队列，绝不
阻塞事务；溢出时用最新事件替换陈旧待处理事件，并在 `Dropped` 中精确报告遗漏数量。
`AfterSequence` 可续接消费，并在历史已淘汰时报告缺口。目前发布打开、恢复、已提交的
Apply/Undo/Redo 变更、保存开始/进度/完成/失败、恢复 WAL Sync 失败/恢复和关闭事件。
进度事件通过 `PersistenceProgress.OperationID` 关联，并能区分提交前失败与提交后永久
fault。多个并发 `Close` 调用者等待同一个资源退役屏障并得到相同结果。

`document/coordinate` 为一个固定 Snapshot revision 构建不可变索引。checkpoint 只落在
UTF-8 边界，因此 byte/line/rune 查询最多读取一个有界窗口。每个 Index 自有按字节严格
限制的不可变窗口 LRU：默认 1 MiB、硬上限 256 MiB，也可以关闭；Stats 报告驻留字节、
条目数和命中/未命中。`ChangeMap` 按一次提交中替换的先后顺序变换 Anchor 和范围，并
明确定义插入边界的 before/after 粘性。通过 Session 创建的坐标索引在 `Close` 前持有
对应 Snapshot lease。

`coordinate.Rebuild` 和 `RebuildOwned` 接收从旧 Index 到新不可变 Source 的精确
ChangeMap 链，验证两端 revision 和长度，并继承 checkpoint/cache 策略。它保留所有编辑
之前的前缀，根据顺序映射推导未触碰旧/新后缀的对应边界，只扫描到第一个真正有收益的旧
后缀 checkpoint，再以新扫描出的 seam 校准并平移后续 byte/rune/line/column 状态；单独
一个 EOF 标记不会伪报为后缀复用。证明依赖公开契约：新 Source 必须正是 ChangeMap
描述的结果；不匹配的 Source 是非法输入，不会靠猜测修正。Stats 分别报告前缀/后缀
checkpoint 复用量和实际解码字节数。

Session 创建的 Index 带有不可由调用方 Options 替换的 opaque lineage。
`Session.RefreshCoordinateIndex` 会校验 lineage，并把保留的映射链与当前 Snapshot 原子
取得；历史已淘汰时明确失败，不会用无关前缀静默重建。`ChangesBetween` 支持真实可观察
revision 边界间的正向与反向查询，原子批次内部 revision 会被拒绝。`TransformAnchors`
与 `TransformRanges` 在预算内原子变换 Anchor/范围批次，不返回部分结果，其 Context
版本支持处理中取消。单个或组合 ChangeMap 最多 1,048,576 个 edit，一次批量变换最多
16,777,216 个 edit×anchor 步骤；`ComposeAll` 一次校验、一次复制整条链，避免保留历史
逐段组合的二次复杂度。`coordinate.Annotation[T]` 只携带内核绝不解释的宿主值。

`document/virtual` 为一个不可变 UTF-8 Source revision 建立确定性的逻辑 Page 表。
Page 在目标大小后优先选择 LF，并在硬上限前强制落到 UTF-8 边界。Fragment 发布使用
generation compare-and-swap；`IndexedThrough` 能区分“已分析但没有 Fragment”的 gap
和仍未索引的后缀。窗口可按 byte、Fragment ID 或宿主定义的非负定点 `Measure` 寻址，
并同时受 bytes、pages、不同 Fragment 数和 Measure 硬预算约束。巨型 Fragment 会拆成
continuation Page，但内核不会按字节比例猜测其 Measure。`Session.VirtualPager` 持有
Snapshot lease，直到调用方 `Close`。

`Session.Compact` 只合并同 Source 且物理连续的 Piece，并把 undo store 重写为仍被历史
引用的数据。`CompactOptions.CheckpointJournal` 会先明确保存选定 revision，再重基追加式
journal；未提交 WAL 绝不会被原地改写，因为那会破坏崩溃原子性或 revision 身份。已经
发出的不可变 Snapshot 仍可继续读取。

实现不变量和文件格式细节见 [MODULES.md](MODULES.md)。

## v0.3.0 破坏性变化

- 删除 recovery v1、单 replacement frame、root frame 及其导出 API；
- v2 使用 `.docengine-journal-v2`、`DOCLOG02` 和 `DOCJNL02`，旧 journal 不读取、
  不迁移，也不属于 v2 扫描命名空间；
- `recovery.Fingerprint` 改为基础长度、真实路径 SHA-256 和完整内容 SHA-256；
- `ReplayResult` 返回原子 batch，不再返回旧逻辑 frame；
- 新增 `document.OpenContext`、`Metadata.ResolvedPath`、耐久性/故障状态、
  `Session.Fault`、`document.ErrFaulted` 和 `save.DurabilityError`。
- `SaveContext`、`CommitAtLeastContext`、`UndoContext`、`RedoContext` 与
  `CloseContext` 明确取消边界；关闭调用可以超时返回，但唯一的清理屏障会继续执行。

1.0 之前不承诺兼容性。

## 测试

当前每个 package 都强制 100% 语句覆盖率，并包含二十四个 Go fuzz target：

- Piece Tree 参考模型、并发 Snapshot/edit、压缩/Snapshot 保留与自动压缩策略；
- v2 文件头、操作 decoder、回放韧性和 journal 状态机；
- Session 状态机、并发保存/编辑、崩溃恢复、journal 配额原子性、跨 generation
  生命周期预算与 UTF-8 编辑边界；
- 可续接事件历史、订阅溢出和关闭状态机；
- 有界 ChangeMap 历史的保留、淘汰、反向查询与组合状态机；
- UTF-8 坐标参考模型、ChangeMap 组合性质，以及增量/全量索引等价性；
- 逻辑 Page 分区、UTF-8 重组、Fragment 窗口参考模型和 Pager generation 状态机。

测试覆盖非法及逐字节截断批次、状态发布回滚、同长度同 mtime 外部篡改、全文件及
跨缓冲区 UTF-8、符号链接重定向、并发编辑/保存/恢复、平台耐久性故障和提交后的
只读状态，以及配置限制、并发共享运行时目录、marker 文件锁孤儿回收、保守清理、
存活 undo 引用重映射和不破坏 Snapshot 的 Piece 压缩、精确 journal 配额拒绝、自动
checkpoint 失败退避，以及带/不带并发编辑时分别在替换前后的真实子进程退出。生命周期测试
还覆盖精确 lease 饱和、64 路并发抢占、关闭唤醒等待保存、每个提交前检查点取消、超时
关闭继续清理，以及自动 checkpoint 被取消后的恢复。事件测试还覆盖精确丢失计数、
续接游标、保存失败阶段和进度、journal Sync 失败/恢复、队列溢出后的最终关闭事件、
并发发布/退订，以及多个调用者等待同一个关闭屏障。

v0.3.0 发布测试已在 Windows 本机和 WSL 2 Debian 的原生 Linux 临时目录执行。
两端所有 package 均达到 100% statement coverage，
`-race -shuffle=on -count=3` 全部通过，三个 fuzz target 均至少运行 30 秒，未发现
实现层反例。

完整 v0.4 发布套件已在 Windows 本机和 WSL 原生 Linux 目录验证：五个 package 继续
保持 100% statement coverage，三轮 shuffle race 全部通过，涉及的九个
Session/event/change-history/coordinate fuzz target 在两端分别运行 10 秒并通过。

首个 v0.5.0 实现已在 Windows 本机验证，六个 Linux 测试二进制也全部交叉编译成功。
随后 v0.5.1 正确性套件在 Windows 本机和 WSL 2 Debian 的原生 Linux `/tmp` 目录完整
执行：两端六个 package 均达到 100% statement coverage，全仓三轮 shuffle race
通过，四个虚拟化 fuzz target 和新增的 Session/Pager 生命周期 fuzz target 各运行
10 秒通过。

v0.5.2 Piece Tree 维护套件在 Windows 本机与 WSL 2 Debian 原生 Linux `/tmp` 目录执行：
两端六个 package 均保持 100% statement coverage，全仓三轮 shuffle race 通过，四个
Piece Tree fuzz target 各运行 30 秒；自动压缩边界测试还连续运行 100 次，提交的四类
store benchmark 也在两端实际执行。

v0.5.3 Recovery/Save 套件在 Windows 本机与 WSL 2 Debian 原生 Linux `/tmp` 目录执行：
两端六个 package 均保持 100% statement coverage，全仓三轮 shuffle race 通过；四个
recovery fuzz、并发保存、崩溃恢复和 journal quota fuzz 各运行 30 秒。真实子进程崩溃
矩阵在每个平台连续通过 20 次，checkpoint/配额边界连续通过 100 次，Recovery/Save/
Session 基准也在两端实际执行。

v0.5.4 Session 生命周期套件在 Windows 本机与 WSL 2 Debian 原生 Linux `/tmp` 两端
通过：六个 package 均保持 100% statement coverage，全仓三轮 shuffle race 通过；
lifecycle budget、Session state、并发保存、崩溃恢复与 Session/Pager 五个 fuzz target
各运行 30 秒。核心 lease/关闭/取消竞争连续通过 100 轮，详细提交前矩阵通过 30 轮。
Snapshot lease 获取在 Windows 约 447–467 ns、Linux 约 347–353 ns（368 B、4 alloc），
4 MiB Session Save 分别约 49–50 ms 与 10–11 ms。

v0.5.5 Coordinate/ChangeMap 套件在同一 Windows 与 WSL 原生 Linux 矩阵通过：六包继续
保持 100% statement coverage，全仓三轮 shuffle race 通过，三个 coordinate fuzz 在
每端各运行 30 秒。缓存/后缀/取消/最大历史边界通过 100 轮普通重复与 10 轮 race 重复。
缓存命中的 64 KiB 窗口查询保持零分配，关闭缓存时每次约分配 72 KiB；4 MiB 文档中部
编辑的增量重建在 Windows 约 0.39–0.47 ms、Linux 约 0.323–0.339 ms，全量构建分别约
27–29 ms 与 20.6–21.9 ms。256-map `ComposeAll` 只分配一次，逐段组合为 256 次。

常规检查：

```bash
go mod verify
gofmt -l .
go vet ./...
go test ./...
go test -race -shuffle=on -count=3 ./...
```

Fuzz：

```bash
go test ./document/store -run=^$ -fuzz=FuzzTreeMatchesReference -fuzztime=30s
go test ./document/store -run=^$ -fuzz=FuzzTreeConcurrentReadDuringEdits -fuzztime=30s
go test ./document/store -run=^$ -fuzz=FuzzTreeCompactionPreservesSnapshots -fuzztime=30s
go test ./document/store -run=^$ -fuzz=FuzzTreeAutoCompactionMatchesReference -fuzztime=30s
go test ./recovery -run=^$ -fuzz=FuzzJournalDecoders -fuzztime=30s
go test ./recovery -run=^$ -fuzz=FuzzJournalStateMachine -fuzztime=30s
go test ./recovery -run=^$ -fuzz=FuzzJournalBatchOperationsDecode -fuzztime=30s
go test ./recovery -run=^$ -fuzz=FuzzJournalReplayResilience -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzSessionStateMachine -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzSessionConcurrentSaveEdit -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzSessionCrashRecovery -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzSessionJournalQuotaIsAtomic -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzSessionLifecycleBudgets -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzUTF8ReplacementBoundaries -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzEventHubStateMachine -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzChangeHistoryStateMachine -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzVirtualPagerSessionLifecycle -fuzztime=30s
go test ./document/coordinate -run=^$ -fuzz=FuzzIndexMatchesUTF8Reference -fuzztime=30s
go test ./document/coordinate -run=^$ -fuzz=FuzzChangeMapBoundsAndComposition -fuzztime=30s
go test ./document/coordinate -run=^$ -fuzz=FuzzIncrementalIndexMatchesFullBuild -fuzztime=30s
go test ./document/virtual -run=^$ -fuzz=FuzzLogicalPagePartition -fuzztime=30s
go test ./document/virtual -run=^$ -fuzz=FuzzLogicalPagesPreserveUTF8 -fuzztime=30s
go test ./document/virtual -run=^$ -fuzz=FuzzFragmentWindowsRespectRanges -fuzztime=30s
go test ./document/virtual -run=^$ -fuzz=FuzzPagerGenerationStateMachine -fuzztime=30s
```

Windows race 构建需要 GCC 兼容的 MinGW-w64，MSVC 目标的 `cl.exe` 或
`clang-cl.exe` 不能直接用于 Go Windows race build。

## 当前限制

- 失效 Session 回收刻意只识别合法的 Docengine marker/undo 条目；未知文件、损坏 marker、
  符号链接和仍持锁目录都会为安全起见保留；
- 原子替换后的重绑定失败会主动停止写入，必须显式重新打开；
- 若宿主不提供更强文件锁，最后一次 hash 到 replace 之间仍存在无法完全消除的竞态；
- Session 托管的 ChangeMap 历史按事务数有界，revision 已淘汰时必须全量重建；增量索引
  的后缀复用要求新 Source 精确对应 ChangeMap。坐标缓存预算只计算驻留窗口，不包含
  并发 cache miss 正在读取的瞬时缓冲区；
- 文件 watcher 候选变化和未来索引/虚拟化进度事件尚未实现；保存及恢复 WAL 的持久化
  状态跃迁已经发布；
- journal 压缩必须显式建立保存检查点；搜索索引、搜索索引压缩和 Composition 尚未实现；
- Fragment 元数据和逻辑 Page 表受 Page、Fragment、key、任务及缓存配置限制；缓存预算
  不包含活跃任务持有的瞬时副本；内核同时构造的 Window payload 受
  `MaximumTasks × Window.Bytes` 约束。宿主可以继续持有已返回副本，因此还必须把这些
  结果计入自己的内存预算；
- 公开 API 和磁盘格式在 1.0 前仍不稳定。

## 下一步

v0.5.x 维护线下一步补齐事件/压缩与 Virtual 的刷新、进度和性能边界；所有现有模块
收口后，再单独修复已知 Windows journal durability CI 竞态并发布一个小版本。之后
v0.6 才开始格式中立搜索。完整完成度评估、目标架构和后续路线见
[develop.md](develop.md)。
