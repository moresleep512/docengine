# Docengine

[English](README.md) · [发展设计与路线图](develop.md)

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
- 绑定 revision、按 UTF-8 边界建立且查询读取有界的 byte/line/rune 坐标索引，并支持
  由 ChangeMap 驱动的保守 checkpoint 前缀复用；
- 编辑、undo、redo 返回顺序 ChangeMap，并支持带前后粘性的 Anchor；
- Session 托管有界 ChangeMap 历史、正反 revision 查询、带 lineage 校验的索引刷新，
  以及原子批量 Anchor/范围变换和 opaque 泛型 annotation；
- 有界、可续接的 Session 事件流，精确报告慢消费者丢失量，发布保存进度与恢复 WAL
  耐久性跃迁，并提供并发关闭屏障；
- Session 资源限制、journal 同步周期以及 shared/owned 运行时目录策略均可配置并可查询；
- 通过跨平台文件锁保护的 owned Session 崩溃孤儿回收；
- 不破坏 Snapshot 的首版 Piece/undo 压缩，以及通过显式保存检查点完成的 journal 重基。

当前还没有全文搜索、Page/Fragment 虚拟化、多源 Composition、协作、远程存储、
UI 或稳定的 1.0 API。各项能力的格式中立边界见 [develop.md](develop.md)。

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
       /          |          |            \
      v           v          v             v
 document/store recovery document/save document/coordinate
  Piece Tree   v2 WAL      原子替换       索引 / ChangeMap
       \          |          /             /
        +---------+---------+-------------+
                           |
                           v
                 操作系统文件与 io.ReaderAt
```

`document/store` 是最底层。它用指向外部 `io.ReaderAt` 字节范围的 Piece 表示逻辑
正文；持久化随机 Treap 提供结构共享、平均对数编辑、有界读取和不可变 root。

`recovery` 把每个逻辑事务存为一个带校验的 v2 batch。96 字节 `DOCLOG02` 文件头
用规范化真实路径和完整基础内容 SHA-256 绑定 journal；只有 `DOCJNL02` 批次的头、
操作表、payload 和 CRC-32C 全部合法后才会参与回放。损坏尾部可以修复且绝不会暴露
半个事务。

`document/save` 把 Snapshot 流式写入同目录临时文件，同步后做最后一次完整内容冲突
检查，再原子替换目标。POSIX 替换已经成功但父目录同步失败时，会返回类型化
`DurabilityError`，调用方不会把“已提交但耐久性未知”误当成“没有替换”。

`document.Session` 负责 Piece Tree、revision 历史、恢复、Source generation、保存
重基和生命周期。`OpenContext` 单次流式扫描完整文件，同时校验 UTF-8、计算 SHA-256
并统计换行。Metadata 同时报告请求路径和真实路径，保存始终固定到真实目标。若原子
替换后重绑定失败，Session 会保留读取能力并永久禁止继续修改。

`OpenOptions` 中的零值限制会解析为明确默认值：每批 256 个操作、单次插入 1 MiB、
undo 存储 256 MiB、保留 256 个事件和 256 个 ChangeMap、每批最多 65,536 个 Anchor、
journal 每秒同步一次。显式目录默认是 shared；省略 SessionDir 时会创建唯一的 owned
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
UTF-8 边界，因此 byte/line/rune 查询最多读取一个有界窗口。`ChangeMap` 按一次提交中
替换的先后顺序变换 Anchor 和范围，并明确定义插入边界的 before/after 粘性。通过
Session 创建的坐标索引在 `Close` 前持有对应 Snapshot lease。

`coordinate.Rebuild` 和 `RebuildOwned` 接收从旧 Index 到新不可变 Source 的精确
ChangeMap 链，验证两端 revision 和长度、继承 checkpoint 间隔，只复用位于所有顺序编辑
之前的前缀，并重新扫描剩余新内容。无法证明行列状态仍正确的后缀不会被冒险平移。
`Session.RebuildCoordinateIndex` 提供当前 Snapshot lease；Stats 会报告复用 checkpoint
数量和实际扫描字节数。

Session 创建的 Index 带有不可由调用方 Options 替换的 opaque lineage。
`Session.RefreshCoordinateIndex` 会校验 lineage，并把保留的映射链与当前 Snapshot 原子
取得；历史已淘汰时明确失败，不会用无关前缀静默重建。`ChangesBetween` 支持真实可观察
revision 边界间的正向与反向查询，原子批次内部 revision 会被拒绝。`TransformAnchors`
与 `TransformRanges` 在预算内原子变换 Anchor/范围批次，不返回部分结果；
`coordinate.Annotation[T]` 只携带内核绝不解释的宿主值。

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

1.0 之前不承诺兼容性。

## 测试

当前每个 package 都强制 100% 语句覆盖率，并包含十五个 Go fuzz target：

- Piece Tree 参考模型与并发 Snapshot/edit；
- v2 文件头、操作 decoder、回放韧性和 journal 状态机；
- Session 状态机、并发保存/编辑、崩溃恢复与 UTF-8 编辑边界；
- 可续接事件历史、订阅溢出和关闭状态机；
- 有界 ChangeMap 历史的保留、淘汰、反向查询与组合状态机；
- UTF-8 坐标参考模型、ChangeMap 组合性质，以及增量/全量索引等价性。

测试覆盖非法及逐字节截断批次、状态发布回滚、同长度同 mtime 外部篡改、全文件及
跨缓冲区 UTF-8、符号链接重定向、并发编辑/保存/恢复、平台耐久性故障和提交后的
只读状态，以及配置限制、并发共享运行时目录、marker 文件锁孤儿回收、保守清理、
存活 undo 引用重映射和不破坏 Snapshot 的 Piece 压缩。事件测试还覆盖精确丢失计数、
续接游标、保存失败阶段和进度、journal Sync 失败/恢复、队列溢出后的最终关闭事件、
并发发布/退订，以及多个调用者等待同一个关闭屏障。

v0.3.0 发布测试已在 Windows 本机和 WSL 2 Debian 的原生 Linux 临时目录执行。
两端所有 package 均达到 100% statement coverage，
`-race -shuffle=on -count=3` 全部通过，三个 fuzz target 均至少运行 30 秒，未发现
实现层反例。

完整 v0.4 发布套件已在 Windows 本机和 WSL 原生 Linux 目录验证：五个 package 继续
保持 100% statement coverage，三轮 shuffle race 全部通过，涉及的九个
Session/event/change-history/coordinate fuzz target 在两端分别运行 10 秒并通过。

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
go test ./recovery -run=^$ -fuzz=FuzzJournalDecoders -fuzztime=30s
go test ./recovery -run=^$ -fuzz=FuzzJournalStateMachine -fuzztime=30s
go test ./recovery -run=^$ -fuzz=FuzzJournalBatchOperationsDecode -fuzztime=30s
go test ./recovery -run=^$ -fuzz=FuzzJournalReplayResilience -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzSessionStateMachine -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzSessionConcurrentSaveEdit -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzSessionCrashRecovery -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzUTF8ReplacementBoundaries -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzEventHubStateMachine -fuzztime=30s
go test ./document -run=^$ -fuzz=FuzzChangeHistoryStateMachine -fuzztime=30s
go test ./document/coordinate -run=^$ -fuzz=FuzzIndexMatchesUTF8Reference -fuzztime=30s
go test ./document/coordinate -run=^$ -fuzz=FuzzChangeMapBoundsAndComposition -fuzztime=30s
go test ./document/coordinate -run=^$ -fuzz=FuzzIncrementalIndexMatchesFullBuild -fuzztime=30s
```

Windows race 构建需要 GCC 兼容的 MinGW-w64，MSVC 目标的 `cl.exe` 或
`clang-cl.exe` 不能直接用于 Go Windows race build。

## 当前限制

- 失效 Session 回收刻意只识别合法的 Docengine marker/undo 条目；未知文件、损坏 marker、
  符号链接和仍持锁目录都会为安全起见保留；
- 原子替换后的重绑定失败会主动停止写入，必须显式重新打开；
- 若宿主不提供更强文件锁，最后一次 hash 到 replace 之间仍存在无法完全消除的竞态；
- Session 托管的 ChangeMap 历史按事务数有界，revision 已淘汰时必须全量重建；增量索引
  从最早受影响 checkpoint 保守重扫，自动缓存所有权及可证明的后缀复用尚未实现；
- 文件 watcher 候选变化和未来索引/虚拟化进度事件尚未实现；保存及恢复 WAL 的持久化
  状态跃迁已经发布；
- journal 压缩必须显式建立保存检查点；搜索索引、Page/Fragment 虚拟化、索引压缩和
  Composition 尚未实现；
- 公开 API 和磁盘格式在 1.0 前仍不稳定。

## 下一步

v0.4 Session 与坐标地基完成后，v0.5 直接开始格式中立的逻辑 Page/Fragment 虚拟化：
有界 Page 读取、Measure 索引、overscan、continuation page、generation 原子发布及严格
缓存/任务预算。之后再向上实现内置持久化搜索和多源 Composition。完整完成度评估、
目标架构、边界情况和 v0.4–v1.0 路线见 [develop.md](develop.md)。
