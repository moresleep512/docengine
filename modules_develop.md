# Docengine 模块开发历程与设计决策

本文记录 Docengine 从 TypeMD 后端抽离，到 v0.5.1 为止的实际开发顺序、模块形成过程、
关键取舍、测试方法和问题复盘。它回答的不是“现在有哪些 API”，而是“代码为什么最终
长成现在这样”。

三份工程文档各有边界：

- [`MODULES.md`](MODULES.md) 是当前实现的模块参考和不变量说明；
- [`develop.md`](develop.md) 是完成度评估、目标架构和未来路线图；
- 本文是历史与设计决策记录，解释已经发生的演进和背后的原因。

文中版本号表示形成稳定里程碑的 tag；同一阶段可能包含 tag 前的多个提交。项目在 1.0
以前允许破坏性改动，因此早期方案会被完整删除，而不是永久背负兼容层。

## 0. 先确定边界：从 TypeMD 后端抽离

### 当时的问题

原始代码服务于 TypeMD，后端同时知道 Markdown 块、产品元数据、SQLite 搜索、Wails
绑定和编辑器布局。这些能力放在同一个后端里可以快速交付产品，但无法单独验证“文档
存储和编辑内核”是否正确，也无法被其他宿主复用。

抽离工作的第一步不是复制更多代码，而是删除业务认识。Docengine 最终只允许理解：

- 字节和 UTF-8；
- BOM、换行、byte/line/rune 坐标；
- revision、范围替换、Snapshot 和抽象 Source；
- 本地文件的恢复、保存和资源生命周期。

Markdown、JSON、代码语法、DOM、像素布局、渲染命令和产品工作流全部留给宿主。
这条边界决定了后续 Page、Fragment、搜索和多源编排也必须使用格式无关接口。

### 为什么先允许源码不完整

抽离时宁可暂时留下不能独立工作的接口，也不把 TypeMD 的业务依赖伪装成“通用层”。
如果一开始以编译通过为最高目标，很容易把 Markdown block、应用目录或 Wails 事件重新
包一层后带进内核。先删除，再逐层恢复可验证能力，能让依赖方向从第一天就保持清楚。

### 形成的不变量

底层包不能反向依赖 `document.Session`，更不能依赖宿主：

```text
document/store ------+
document/save -------+--> document.Session --> host adapters
recovery ------------+
document/coordinate -+
```

测试也从业务示例转向可证明的不变量：相同输入必须得到相同字节、失败不得发布半个状态、
旧 Snapshot 不得因新编辑失效、磁盘提交状态必须能被明确描述。

## 1. v0.1：建立最小可独立验证的地基

这一阶段对应初始提交、`Establish module and harden piece tree`，以及首个文档版本
`v0.1.0`。

### 1.1 Piece Tree 为什么是最底层

大型文件不能在每次编辑时完整复制到内存。`document/store` 因此不保存一份连续字符串，
而是保存 Piece：每个 Piece 只描述某个 `io.ReaderAt` Source 中的一段物理字节范围。
初始正文引用基础文件，新增正文引用追加式 journal。

树采用持久化随机 treap，而不是普通切片或可变平衡树，原因有三点：

1. 子树缓存逻辑字节数，按偏移定位和替换只依赖树高；
2. 编辑时只复制修改路径，旧 root 自然成为不可变 Snapshot；
3. 随机优先级实现紧凑，不需要把复杂旋转和父指针暴露给上层。

拆分 Piece 时必须继承原节点优先级。早期边界审计发现，如果左右碎片重新生成优先级，
拆分回溯时碎片可能越过祖先，破坏 treap 的 heap 不变量。修复后把“拆分不改变局部优先
关系”写进测试，而不是只比较最终文本。

### 1.2 Snapshot 为什么同时捕获 root 和 Source

仅保存不可变 root 不够。root 中的 Piece 仍然引用外部文件或 journal；如果 Session 保存
后关闭旧文件，Snapshot 虽然结构还在，读取却会失败。

因此 `Snapshot` 捕获 root 与 Source 绑定，`sourceGeneration` 再用引用计数提供
`SnapshotLease`。Session 保存后可以切换到新 generation，但旧基础文件和旧 journal
必须等最后一个 lease 关闭后才退役。这里把资源生命周期放在 `document`，而没有让
`store` 擅自关闭宿主提供的 `io.ReaderAt`，保持了包的所有权边界。

### 1.3 第一版恢复和保存

最初保留了从 TypeMD 演化来的 recovery v1 frame，包括单次 Replace 和 root 概念。
它让崩溃恢复路径先跑通，但还不能准确表达多操作事务。保存则建立了同目录临时文件、
同步、原子替换和 generation 重绑定的基本顺序。

这一版的意义是建立端到端骨架，不是承诺磁盘格式。1.0 前不做兼容承诺，为后续彻底删除
v1 留出了空间。

### 1.4 测试点

- 空树、头尾插入、跨 Piece 删除和无操作替换；
- 负数、越界和 `int64` 溢出；
- Snapshot 在后续多轮编辑后保持原字节；
- Source 缺失和短读错误；
- Windows 与 POSIX 的基础打开、替换路径。

这一阶段给上层提供的是“可持久引用、可快照、可按范围编辑的字节地基”。

## 2. v0.2：把单次编辑提升为原子事务

`Harden atomic batches and expand boundary tests` 和 v0.2 系列提交把系统从“可以编辑”
推进到“失败时也能说明发生了什么”。

### 2.1 为什么 `ApplyBatch` 必须是唯一事务边界

宿主的一次语义操作可能包含多个范围替换。如果逐个调用 Replace 并立即发布，中间一步
失败会留下半个操作，Undo、journal 和事件也会看到不同状态。

`Session.ApplyBatch` 因而采用先准备、后发布：

1. 校验 expected revision、操作数量、范围和 UTF-8 边界；
2. 从旧 Snapshot 读取需要保存的删除内容；
3. 在临时 Tree/暂存 Source 上顺序应用全部操作；
4. 把一个 group 的完整批次追加到 recovery；
5. 只有所有步骤成功，才切换 Tree、revision、undo 和 pending 状态。

这不是数据库式分布式事务，但在一个 Session 内保证“全有或全无”。journal 的 group
也必须与内存事务一致，否则崩溃恢复会暴露运行时从未公开过的中间态。

### 2.2 Undo 为什么使用独立磁盘存储

Undo 需要保留被删除的大段文本。如果直接把删除内容放进 Go heap，大文件反复编辑会让
内存预算失控；如果引用基础文件，又会在保存切换 generation 后失效。

因此 undo history 只保存范围和 `textRef`，正文追加到 Session 自己的 undo store。
forward/inverse 操作都引用稳定偏移。Redo 不是重新解释宿主命令，而是应用已记录的逆向
事务，从而保持 revision 和恢复模型一致。

### 2.3 原子保存的跨平台差异

POSIX 使用同目录临时文件、文件同步、rename 和父目录同步；Windows 使用
`ReplaceFileW`，并带 write-through 语义。两端不能共用一个假想的“rename 就完成”
实现，因为 Windows 的共享标志、替换参数和错误传播完全不同。

保存期间允许新编辑继续发生，所以保存捕获的是一个不可变 Snapshot。磁盘成功后，
Session 以已提交内容建立新 base，再把保存期间到达的 pending group 原样复制到新
journal 并重放。这就是后来的 save rebase。

### 2.4 100% coverage 为什么在此时成为门禁

普通成功路径已经不能继续提高可靠性，真正危险的是第 N 个系统调用失败：append 已写但
未发布、临时文件已创建但 replace 失败、replace 已成功但 reopen 失败等。

v0.2 引入可注入 operations 和系统化 fault tests，对 stat、open、write、sync、rename、
reopen、Tree 构造和清理逐点失败。100% statement coverage 不是“没有 bug”的证明，
但它强迫每个错误分支至少被实际执行，并为后来寻找语义死角提供基线。

### 2.5 实际出现的问题

- Linux 独有错误分支没有被 Windows 测试覆盖，导致 coverage job 失败；v0.2.1 和
  v0.2.2 分别补齐 Linux Session 和绝对路径失败测试。
- Go 的 Windows race 构建不能使用 MSVC 或 `clang-cl`，需要 GCC 兼容的 MinGW-w64。
  因此本机普通测试和 race 工具链被明确区分。
- 只检查成功结果的测试无法发现资源泄漏，于是失败测试同时核对临时文件、句柄和目录。

这一阶段形成了后续所有功能共同遵守的发布原则：先构造完整新状态，再一次性使其可见。

## 3. v0.3：破坏性升级 recovery 和文件持久化

v0.3.0 是第一次明确的破坏性版本。旧 recovery v1 不迁移、不识别，也不保留 decoder；
新格式使用独立后缀和 magic，让旧文件自然不进入 v2 命名空间。

### 3.1 为什么完整删除 v1

v1 把批事务伪装成一组旧 Replace frame，并保留 root frame。继续兼容会让每个新功能都
同时回答两套 revision、原子性和截断语义，测试矩阵也会永久翻倍。

项目尚未到 1.0，没有需要保护的稳定下游，因此选择删除旧导出 API、magic、decoder 和
fixture。这个决定把“原子 batch 是唯一恢复单位”变成磁盘级不变量，而不是运行时约定。

### 3.2 recovery v2 为什么这样布局

v2 文件头保存版本、头尺寸、基础文件长度、规范化真实路径 hash、完整基础内容 hash、
保留字段和 CRC-32C。批次记录保存首 revision、group、操作数、payload 长度和覆盖整个
批次的 CRC。

操作表与 payload 一起校验，使回放只能看到完整 batch。尾部截断或 CRC 损坏时，只回放
此前完整批次并修复坏尾；文件头、基础 fingerprint 或歧义 journal 出错时，文件会被
隔离并阻止本次打开。这里宁可拒绝自动猜测，也不把错误 journal 应用到错误正文。

所有长度、数量、revision 和偏移先做上限及溢出检查。攻击面不只来自恶意文件，断电和
部分写同样会生成任意字节组合，所以 decoder 必须把输入当成不可信数据。

### 3.3 `OpenContext` 为什么扫描完整文件

只检查文件前 64 KiB 会漏掉后部非法 UTF-8，也无法生成完整内容 fingerprint。
`OpenContext` 因而以固定 256 KiB 缓冲区单次流式扫描：

- 跨块验证 UTF-8；
- 计算包含 BOM 的完整 SHA-256；
- 统计全文换行风格；
- 响应 Context 取消；
- 扫描前后核对文件句柄和路径身份。

缓冲区有界，所以文件大小不会线性增加 heap。BOM 属于磁盘身份，但不属于逻辑正文，
因此 hash 包含 BOM，Piece Tree 的 base Piece 从 BOM 后开始。

### 3.4 为什么固定符号链接的真实目标

打开时解析符号链接一次，并同时保存请求绝对路径与真实路径。Session 后续始终写入这个
固定目标；链接后来重定向不能悄悄改变保存对象。recovery fingerprint 也使用规范化真实
路径，避免同一文件通过别名产生不兼容 journal。

Windows 路径还需要大小写无关和分隔符规范化。v0.3.1 的 CI 暴露了恢复路径在 Windows
别名下不稳定的问题，随后增加平台路径规范化和专门回归测试，而没有把 Windows 规则
污染到 POSIX。

### 3.5 保存为何区分“提交”和“耐久”

rename/ReplaceFileW 成功表示新正文已成为目标文件，但父目录同步仍可能失败。此时返回
普通错误并保持旧 committed revision，会诱导宿主重试整个替换，甚至覆盖新修改。

因此 POSIX 目录同步失败返回 `DurabilityError`，`CommittedRevision` 仍前进，同时设置
`DurabilityUncertain`。无正文变化的下一次 Save 只重试目录同步。恢复 journal 的后台
Sync 失败则使用独立的 `RecoveryDurabilityUncertain`，因为它影响的是未提交编辑能否抗
掉电，而不是基础文件是否已经替换。

### 3.6 为什么需要永久只读 fault 状态

替换成功后还要 reopen base、创建新 Tree、建立 journal 并重放并发编辑。任何一步失败
都不能假装磁盘没有提交，也不能继续在来源不完整的 Session 上修改。

Session 因此保存原始 cause，标记 `PersistenceFaulted` 并永久禁止 Apply/Undo/Redo/Save；
读取、Snapshot、Metadata、Fault 和 Close 仍然可用。显式故障状态比尝试隐式回滚一个
已经发生的文件系统替换更诚实，也更容易让宿主恢复。

### 3.7 v0.3.2：从例子测试转向性质和状态机

固定案例无法覆盖 Piece 分裂组合、随机崩溃位置和并发 save rebase。v0.3.2 增加：

- Piece Tree 与内存 byte slice 的参考模型；
- journal decoder、回放韧性和随机 truncate/bit-flip 状态机；
- Session Apply/Undo/Redo/Save/恢复状态机；
- 保存中并发编辑及旧 Snapshot 读取的 race/fuzz；
- Windows ReplaceFileW 参数、瞬态错误重试和清理性质测试。

fuzz 的判断标准不是“不能 panic”而已，还要比较 reference model、revision、journal 原子
批次和 Snapshot 内容，确保随机输入不能暴露半个状态。

## 4. v0.4：从存储内核走向可嵌入 Session

v0.3 解决了正文如何安全存在，v0.4 开始解决宿主如何稳定消费 revision、坐标、事件和
资源策略。这个顺序很重要：如果先做虚拟化或搜索，它们会建立在不稳定的 revision 和
生命周期之上。

### 4.1 坐标索引为什么独立于 Piece Tree

Piece Tree 的职责是字节范围和 Snapshot，不应同时承担 line/rune 策略。`coordinate`
因此基于不可变 `io.ReaderAt` 建立 byte、line、rune checkpoint，查询只读取目标附近的
有界区间。

索引携带 revision 和 opaque lineage。旧 Index 只能通过明确的 ChangeMap 链刷新到同一
Session 的新 Snapshot；来自其他 Session 的结构即使长度相同也不能复用。增量重建只
复用所有编辑之前可证明安全的 checkpoint 前缀，无法证明的后缀重新扫描。

这里选择保守复用而不是激进平移，因为换行或多字节 UTF-8 的一次变化可能使后续全部
坐标偏移。Stats 会报告复用 checkpoint 和实际扫描字节，性能优化因此可以被测试。

### 4.2 ChangeMap、Anchor 和 Range

每次成功事务返回顺序 ChangeMap，描述每一步替换发生时的坐标空间。Anchor 带前后
affinity，解决插入正好发生在锚点位置时应该粘向哪一侧。Range 和 opaque Annotation
只组合通用区间，不理解高亮、诊断或 Markdown 节点的业务含义。

ChangeMap 支持组合和反转，但不能无限保留。Session 后来加入有界 change history，
提供正反 revision 查询；过期和中间 revision 不可用使用类型化错误，而不是生成猜测
映射。恢复后的 Session 从恢复完成 revision 建立新的历史窗口，因为此前运行时映射并
不存在。

### 4.3 事件流为什么必须有界且不阻塞事务

宿主需要监听打开、恢复、编辑、保存进度、WAL Sync 和关闭，但慢订阅者不能持有 Session
锁或阻塞编辑。

事件 hub 为每个订阅者维护有界队列，溢出时丢弃陈旧待处理事件并在下一事件中精确报告
`Dropped`。消费者看到缺口后必须从匹配 revision 的 Snapshot 重建派生状态，而不是
继续盲目增量应用。`AfterSequence` 用于续接保留历史，`FutureOnly` 用于只监听未来。

Close 发布最终事件并形成资源屏障；多个并发 Close 调用者等待同一结果，避免重复关闭
句柄或得到不同错误。

### 4.4 配置和目录所有权

早期零值选项隐含了太多策略。v0.4 将批次、插入、undo、事件、ChangeMap 和 Anchor
上限解析为不可变 `SessionConfig`。显式目录默认 shared，自动创建目录才是 owned。

owned Session 目录使用持锁 marker。清理器只删除 marker 合法、未持锁、达到时间阈值
且全部内容可识别的目录；遇到符号链接、未知文件、损坏 marker 或仍持锁进程一律保留。
这里刻意不用递归删除，因为运行时目录中出现宿主文件时，数据安全高于“清理干净”。

### 4.5 压缩为什么分成三类

- Piece compaction 只合并同 Source、物理偏移连续的相邻 Piece，不读取或改写正文；
- undo compaction 重写仍被 undo/redo history 引用的字节，并重映射 `textRef`；
- journal 不能原地压缩未提交 WAL，否则会破坏崩溃原子性和 revision 身份，因此只有显式
  `CheckpointJournal`：先保存选定 revision，再以新基础重建 journal。

旧 Snapshot 继续持有旧 generation，压缩和保存都不能提前删除其 journal。这个约束通过
Snapshot 生命周期测试，而不是依靠实现注释。

### 4.6 v0.4 的测试组合

- 坐标索引与逐字节 UTF-8 reference model；
- ChangeMap 组合、反转、Anchor affinity 和 history 淘汰状态机；
- 事件溢出精确计数、续接游标、并发退订和关闭屏障；
- owned/shared 目录、marker 锁和保守孤儿回收故障注入；
- Piece/undo/journal checkpoint 压缩与旧 Snapshot；
- Windows 与 WSL 原生 Linux 的 100% coverage、race 和相关 fuzz。

这一阶段形成了可供未来 Page、Fragment、搜索和 Composition 使用的 revision/坐标/事件
地基，但没有提前把任何 Markdown 或像素布局放进内核。

## 5. v0.4.1：在 100% coverage 之后继续找语义死角

100% statement coverage 只说明每行执行过，不说明跨模块时序正确。v0.4.1 专门寻找
“单个模块都有测试，但组合后可能出错”的场景。

### 5.1 真实进程退出与 marker 锁

原测试通过手工 unlock/close 模拟崩溃，不能证明 Windows `LockFileEx` 和 POSIX `flock`
会在进程未清理 marker 的情况下由操作系统释放。

新测试启动真实子进程，让它持锁并留下 undo 文件；父进程先验证活跃目录不会被回收，
再让子进程不调用 marker close 直接退出，最终确认锁释放且孤儿目录可被安全回收。

### 5.2 并发保存事件和 revision 关联

已有并发保存 fuzz 比较正文，却没有验证事件中的 target revision、当前 revision 和
committed revision。新测试在 Save 捕获 rev1 后阻塞，期间提交 rev2，再检查事件严格为：

```text
SaveStarted(target=1) -> Changed(current=2) -> SaveProgress(target=1)
-> Saved(current=2, committed=1, dirty=true)
```

磁盘只能包含 rev1，内存必须包含 rev2。这样事件消费者不会把“保存完成”误解为当前全部
编辑都已落盘。

### 5.3 部分写失败和 checkpoint/Snapshot 组合

写入中途失败的测试确认磁盘未替换、Session 仍可写且 progress 不会虚报完成。另一个
测试把 journal checkpoint、并发新编辑、旧 Snapshot、Undo/Redo 放在同一时序中：旧
journal 必须在 Snapshot 关闭前保留，新编辑必须重基到新 journal，history 在压缩后仍
能往返。

Piece compaction 又增加状态 fuzz，随机编辑后验证当前正文、所有旧 Snapshot、Piece 数
单调和二次 Compact 幂等。CI fuzz target 因此增加到十六个。

## 6. v0.4.2：修复同大小、同 mtime 的扫描末尾改写

### 6.1 原测试为什么可能间歇失败

`TestOpenRejectsChangeAtEndOfScan` 在最终 `stat(path)` 时把文件从一组字节改成同长度另一
组字节。旧实现用 `size + mtime(ns) + SameFile` 比较扫描前后状态，通常因为写入推进
mtime 而返回 `ErrExternalChange`。

但 `SameFile` 只证明是同一个文件对象，不证明内容没有变化。如果文件系统时间分辨率、
调度窗口或外部程序让最终 mtime 与初始值相同，旧实现会接受扫描得到的旧 hash，同时
让 Piece Tree 读取已经改变的 base。

在当前 NTFS 上，原测试连续 100 次都通过，所以它看起来稳定；加入写后调用 `Chtimes`
恢复原 mtime 的确定性测试后，修复前 Windows 20/20、WSL 原生 Linux 5/5 都错误打开
成功。这证明问题不只是理论上的测试抖动。

### 6.2 为什么没有选择无条件第二次 SHA-256

第二次完整读取可以比较两次 hash，但会把每次打开大文件的磁盘读取近似翻倍。Docengine
的目标正是让大型本地文档保持有界内存和可接受打开成本，因此不能为一个极窄竞态无条件
支付 O(n) 的第二遍 I/O。

最终选择常数时间变更代际：

- Windows 对已打开句柄在扫描前后查询 `FILE_BASIC_INFO.ChangeTime`；
- Linux、Darwin 和 BSD 从已有 `stat` 结果读取 `ctime`；
- 原有 size、mtime、SameFile 和完整 SHA-256 全部保留；
- 最终路径 stat 先执行，再读取句柄最终状态和变更代际，使 stat 回调期间发生的原地改写
  也被包含在检测窗口内。

Linux/BSD 不增加系统调用；Windows 只增加两次常数时间句柄查询。内容依然只扫描一遍。
专门的性能回归使用超过两个扫描块的文件，断言打开扫描和保存前身份扫描都只发生三个
`ReadAt`，防止以后无意退化成双遍读取。

### 6.3 平台实现和测试

Windows 测试验证 FILE_BASIC_INFO 的 class、结构大小、ChangeTime 提取和 API 错误传播；
POSIX 测试覆盖系统元数据不可用的兼容路径。公共测试注入初始/最终代际捕获失败，并覆盖
available/unavailable、相等和不等组合。

最终验证包括：

- Windows 与 WSL 原生 Linux 五个 package 100% statement coverage；
- 两端三轮 `-race -shuffle=on -count=3`；
- 两端同 mtime 回归各连续 100 次；
- Darwin、DragonFly、FreeBSD、NetBSD、OpenBSD 和 Linux 交叉编译；
- 全仓 `go vet`、普通测试和格式检查。

### 6.4 仍然存在的物理边界

任何有限次数的校验都不能阻止外部程序在“最终检查完成之后”立即原地改写文件；拥有足够
权限的程序也可能主动伪造 change time。要消除这类对抗性写入，只能使用私有基础快照或
强制排他锁，代价分别是额外磁盘 I/O/空间，或破坏与其他编辑器的兼容性。

当前模型面向本地单写者文档内核：变更代际封住扫描期间的 metadata-preserving 改写，
保存前完整 hash 防止覆盖已观察到的外部内容变化；未来文件 watcher 和可注入冲突策略
仍属于 `develop.md` 中的后续工作。

## 7. v0.5：把 Snapshot 地基变成通用虚拟化

### 7.1 为什么先做逻辑 Page

搜索、渲染调度和多源编排都不能以“整份文档已经在内存”为前提。v0.5 因此先引入
`document/virtual`：它只依赖 immutable `ReaderAt` Source，不导入根 `document`，再由
`Session.VirtualPager` 转移 Snapshot lease。旧 Pager 在后续 edit/save 后仍读取旧
revision，新 Pager 才观察新 revision，这与 coordinate Index 的生命周期完全一致。

逻辑 Page 在 target 大小之后等待 LF，但绝不越过 maximum；长行在 UTF-8 boundary 强制
切分。空文档也有一个确定的 `[0,0)` Page。这里没有使用 TypeMD 旧实现的 Markdown block、
`float64` height 或 `strings.ToValidUTF8`：前两者会把格式/像素带回内核，后者会静默修改
正文。

### 7.2 Fragment publication 为什么需要水位和双 generation

Fragment 只包含 opaque ID/DataKey、byte range 和 `Measure int64`。同一批范围有序、
不重叠且必须落在 UTF-8 boundary，但允许 gap。`IndexedThrough` 表达 Provider 已分析到的
前缀，所以水位内 gap 是“已知没有 Fragment”，水位后的 suffix 才是未知；单独一个
`Complete bool` 无法表达这个差别。

Pager revision 固定正文身份，Fragment generation 固定派生索引身份。Provider 在没有
Pager/Session 锁的情况下构建结果，发布时用 `BaseGeneration` CAS；慢结果若落后就返回
stale，不能覆盖新结果。所有 key 都 clone 后保存，避免短 substring 让一个一字节 key
长期持有数百 MiB backing allocation；Fragment 数、key bytes、单 Fragment Measure 和
累计 Measure 都有独立检查。

### 7.3 Measure 与 continuation 的边界

宿主只给出整个 Fragment 的 Measure，没有给 continuation 内部的测量分布。按 byte 比例
拆 Measure 看似方便，却是在内核伪造布局。最终设计让巨型 Fragment 拆成多个有界 I/O
Page，每页重复父 Fragment 的原子 Measure interval，并允许按 continuation 定位。
byte、Fragment ID、Measure 三类窗口都携带 revision/generation，支持非对称 overscan，
并同时执行 byte/page/distinct-fragment/Measure 四项硬预算。

零 Measure、连续零值和 Measure 边界必须有确定 affinity：Before 选择左侧，After 选择
右侧；全零序列在同一点分别夹到末端和起点。请求超界、索引尚不可用和 generation 过期
使用不同错误，宿主不需要猜测是等待、重建还是修正参数。

### 7.4 缓存、任务和关闭不是附属实现

Page payload 使用严格 byte-capacity LRU，命中和返回都复制，调用方不能修改缓存。
`CacheBytes` 只统计驻留 payload；并发 miss 和返回副本的瞬时上限由
`MaximumTasks × Window.Bytes` 约束内核同时构造的 Window；调用返回后宿主持有多久，
属于宿主自己的内存预算。任务 semaphore 满时立即返回 `ErrBusy`，形成明确背压。

`Close` 先阻止新任务，再等待已接纳任务，所有并发 Close 调用共享同一 barrier 和 Source
release 错误。开发中曾出现第二个 Close 提前返回、巨型 Fragment 因把全部 continuation
当作一个 anchor 而永久无法读取、`fragmentIndex + After` 整数溢出、gap Start 落入
多字节 rune、EOF 被误判为 LF 等问题；它们都在 100% 行覆盖之外通过语义复审和定向
并发/边界测试发现。

### 7.5 测试为下一层冻结了什么

六个 package 继续保持 100% statement coverage。virtual 测试覆盖 Page 重组、UTF-8
跨缓冲区、line/continuation、partial/full watermarks、gap fallback、key/Measure overflow、
三类锚点、四项预算、缓存逐出、Provider stale race、任务背压和并发 Close。四个 fuzz
target 分别验证逻辑分区、UTF-8 重组、Fragment 窗口和 generation 状态机，并增加 build、
publish 与 cached-window benchmark。

这层完成后，v0.6 搜索可以复用相同 Page、revision/generation、Context 和预算语义，
不需要重新发明大文件分块或异步结果淘汰协议。

### 7.6 v0.5.1：组合生命周期比单模块覆盖率更重要

v0.5.0 完成后继续审计跨模块时，发现 `Save` 与 `Session.Close` 没有共享同一串行边界：
Save 已捕获旧 generation 的 Snapshot 后，Close 可能先 retire 旧 generation；Save 随后
安装新 generation，却没有任何 Close 再负责回收它。旧 Pager 同时存在时还可能重复释放
旧 generation 的 owner reference。修复让 Close 在整个过程持有 `saveMu`，并让
generation retirement 幂等、允许后续把“保留 journal”升级为“删除 journal”。确定性
测试在 Save 的 snapshot hook 阻塞，随后启动 Close，验证 Save 发布后 Close 回收最终
generation，而旧 Pager 仍可读且旧 journal 只在 Pager.Close 后删除。

同一轮还修复了两个容易被示例测试漏掉的契约。其一，只有 revision/generation/range 的
`PageKey` 会被分页相同的另一个 Pager 接受并读取错误 Source；现在 key 带签发 Pager 的
不透明身份，复制有效，伪造或跨 Pager 使用被拒绝。其二，cache hit 的 `ReadPage` 原先
不会像 miss 一样再次观察 Context；现在任务接纳后统一检查取消。内存文档也从错误的
`MaximumTasks × MaximumPageBytes` 修正为内核同时构造 Window 的
`MaximumTasks × Window.Bytes`，宿主持有的返回副本另计。

测试不只增加行覆盖：并发 Publish 同一 base generation 必须恰有一个完整赢家；多 Page
cache 返回值可修改但不能污染缓存；非法配置与扫描中取消不能泄漏 Snapshot lease；
Pager 要跨 undo/redo、save、checkpoint 和恢复重开保持精确旧 revision；新增 stateful
fuzz 随机组合 edit、save、undo/redo、compact、创建/查询/关闭多个历史 Pager。v0.5.1
最终在 Windows 与 WSL 2 Debian 原生 Linux `/tmp` 两端完成六包 100% statement
coverage、全仓三轮 shuffle race，以及五个相关 fuzz target 各 10 秒验证。

### 7.7 v0.5.2：自动维护 Piece Tree，而不是依赖宿主记得压缩

手动 `Session.Compact` 能证明压缩安全，却不能保证长期运行的宿主会在正确时间调用它。
直接在每次编辑后扫描全树又会把 Piece Tree 的平均对数编辑退化成 O(n)。因此
`document/store` 增加可配置的 Piece 数阈值：零值在 4,096 Piece 触发，只合并同 Source
且物理连续的逻辑邻居；若一次扫描没有收益，下次触发点后移一个完整阈值，Piece 数跌回
基础阈值以下时再复位。宿主可以显式关闭自动策略，但默认 Session 不再无限依赖人工维护。

自动压缩发生在 replacement root 完整构建之后、after Snapshot 发布之前。before
Snapshot、压缩前已发出的任意 Snapshot 及其 Source map 都保持不变。`Tree.Stats` 把
正文长度、Piece 数、换行元数据、有效阈值、下次触发点和自动压缩次数放在同一读锁快照
中，测试和宿主不必组合多个可能跨编辑的查询。

测试覆盖阈值精确触发、第二轮触发、无收益退避、关闭策略、非法配置、阈值饱和、已知/
未知换行统计，以及多 goroutine Snapshot 读取穿过反复自动压缩。新的 stateful fuzz 为
每个输入随机选择小阈值，与 byte slice 参考模型逐步比较 before/after/current Snapshot，
并长期保留旧 root。四类 benchmark 分别固定顺序追加、随机单字节替换、16K 碎片随机
读取和 16K Piece 压缩，防止以后只凭覆盖率声称 Piece Tree 已经成熟。最终 Windows 与
WSL 2 Debian 原生 Linux `/tmp` 两端均保持六包 100% statement coverage、通过全仓三轮
shuffle race，四个 Piece Tree fuzz target 分别运行 30 秒；自动压缩边界连续运行
100 次，四类 benchmark 也在两端实际执行。

## 8. 贯穿所有阶段的设计理念

### 8.1 格式中立不是口号，而是依赖约束

内核可以返回 byte range、revision 和通用 annotation，但不能返回 Markdown heading 或
代码 token。只要底层开始理解一种格式，虚拟化、搜索和组合就会被该格式绑定。

### 8.2 不可变 Snapshot 是并发读的共同语言

保存、坐标构建、搜索和未来 Page 调度都应读取绑定 revision 的 Snapshot，而不是长时间
持有 Session 锁。Source generation 确保结构不可变之外，底层句柄也活得足够久。

### 8.3 原子发布优先于就地修补

ApplyBatch、journal append、save rebase、Index 刷新和 generation 切换都先构造完整结果，
再一次发布。错误路径宁可返回明确失败，也不暴露半个事务。

### 8.4 所有权必须可以回答“谁负责关闭和删除”

Tree 不关闭 Source；generation 管理 base/journal；Session 管理 generation、undo、marker
和后台循环；owned/shared 配置决定目录能否由内核删除。模糊所有权最终都会变成崩溃恢复
或并发 Close 的数据丢失。

### 8.5 已提交、已同步和仍可编辑是三件不同的事

`CommittedRevision`、`DurabilityUncertain`、`RecoveryDurabilityUncertain` 和
`PersistenceFaulted` 分别表达不同状态。把它们压成一个 error 会让宿主无法做正确决策。

### 8.6 性能优化必须有可验证预算

固定扫描缓冲区、单遍 hash、checkpoint 前缀复用、Piece 合并、undo 重写和有界事件/history
都配套统计或测试。没有约束的缓存和“看起来更快”的增量算法不进入内核。

### 8.7 测试从结果示例逐层走向状态空间

当前测试层次是：

1. 单元和边界测试验证局部不变量；
2. fault injection 穿过每个系统调用失败点；
3. property test 与内存参考模型比较；
4. stateful fuzz 组合编辑、保存、恢复、截断和压缩；
5. race 测试验证 Snapshot、事件和保存并发；
6. Windows/WSL 原生文件系统验证平台语义；
7. 真实子进程测试验证仅靠 mock 无法证明的锁生命周期。

100% statement coverage 是最低门槛，不是终点。v0.4.1 和 v0.4.2 都是在覆盖率已经 100%
之后发现的语义缺口。

## 9. 到这里为止，以及为什么下一步不是继续堆编辑 API

截至 v0.5.2，Docengine 已经有 Piece Tree、不可变 Snapshot、原子事务、recovery v2、
跨平台保存、显式故障状态、坐标/ChangeMap、事件、资源策略、压缩，以及格式中立的
Page/Fragment/Measure 虚拟化。

下一阶段应按 `develop.md` 继续完成：

- 持久化、可验证、可取消的原始文本搜索；
- 持久区间集合和多 Snapshot Source Composition；
- watcher、后台任务预算、背压、GC/压缩策略及长期 soak/crash matrix。

这些模块都必须建立在当前 revision、Snapshot、Source 所有权和故障语义之上。先把地基做成
可证明的内核，再向上增加搜索和编排，是整个项目从 TypeMD 抽离后最重要的开发顺序。
