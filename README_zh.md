# Docengine

[English](README.md)

Docengine 是一个实验阶段的 Go 文本文档内核，目标是在不把完整文档加载到内存的情况下，编辑大型本地 UTF-8 文本文件。

它最初从 [TypeMD](https://github.com/moresleep512/TypeMD) 的后端文档引擎中抽离。当前代码已经成为独立的 Go 模块和 Git 仓库，并从核心中移除了 TypeMD、Markdown、索引、搜索和编辑器呈现层的具体策略。

```go
module github.com/moresleep512/docengine
```

## 仓库定位

Docengine 是一个**本地文本文档引擎**，不是应用服务器，也不是完整编辑器。

它负责为桌面应用、CLI、语言工具或未来的服务宿主提供底层存储与持久化能力：

- 通过持久化 Piece Tree 进行磁盘支撑的文本编辑；
- 编辑和保存继续进行时，旧的不可变快照仍可稳定读取；
- 带 revision 检查的替换操作和磁盘支撑的 undo/redo；
- 追加式崩溃恢复；
- 流式、带外部冲突检查的原子保存；
- 文档体积增长时保持前台内存使用有界。

当前仓库**不包含**：

- HTTP、RPC 或 WebSocket 服务；
- 桌面或 Web UI；
- Markdown 解析或渲染；
- 全文搜索或索引；
- 多人协作、OT 或 CRDT；
- 远程存储或数据库存储；
- 已稳定并完成版本化的公共 API。

项目目前处于**早期/实验阶段**。最底层 Piece Tree 已经经过较严格的加固，但事务、恢复和保存语义仍需继续完善，暂时不应视为生产就绪版本。

## 与 TypeMD 的关系

原始实现位于 TypeMD 的私有后端包中。最初抽离 Docengine，是为了让文档内核可以独立演进，不再依赖 Wails、TypeMD 前端、Markdown 块模型或编辑器布局策略。

第一轮清理已经完成：

- 删除 Markdown 块扫描和块元数据索引；
- 删除 SQLite FTS 搜索；
- 删除硬编码的索引构建与发布流水线；
- 删除编辑器虚拟化和估算布局高度协议；
- 替换或删除 TypeMD 专有路径、后缀、持久化 magic、JSON 桥接标签和导入路径。

Docengine 与 TypeMD 目前不会自动同步。本仓库的改动不会影响 TypeMD，除非 TypeMD 后续明确迁移到这个模块。当前 `DOCLOG01`/`DOCJNL01` 恢复格式也有意不兼容旧 TypeMD journal magic。

## 架构

```text
未来宿主：CLI / 桌面应用 / HTTP / RPC
                    |
                    v
              document.Session
       revision、编辑、undo/redo、保存
          /          |             \
         v           v              v
 document/store   recovery      document/save
   Piece Tree      journal        原子替换
         \           |              /
          +----------+-------------+
                     |
                     v
          操作系统文件与 io.ReaderAt 数据源
```

### `document/store`

最底层的数据结构层。它把逻辑文档表示为多个 Piece，每个 Piece 指向外部 `io.ReaderAt` 数据源中的一个字节区间。持久化随机 Treap 提供结构共享、平均对数复杂度编辑、不可变 root 和有界范围读取。

### `recovery`

追加式恢复 journal，包含文件指纹、revision、分组替换 frame、payload CRC-32C 校验、重放和损坏尾部修复。

### `document/save`

把不可变快照流式写入同目录临时文件，完成同步后检查原文件是否被外部修改，再原子替换目标。Windows 使用 `ReplaceFileW`，其他平台使用 `os.Rename`。

### `document`

当前的公共协调层。`Session` 负责 revision、Piece Tree、恢复、磁盘 undo/redo、快照 generation、并发保存、UTF-8/BOM/EOL 策略和资源退休。

更详细的实现设计、不变量、文件格式、限制和已删除模块说明见 [MODULES.md](MODULES.md)。

## 当前已经完成

### 仓库基础

- 独立 Git 仓库和 Go 1.26 模块；
- 公共模块路径：`github.com/moresleep512/docengine`；
- Linux 和 Windows CI；
- 格式检查、vet、单元测试、race 和 fuzz smoke job；
- Go 源码中已移除 TypeMD 产品依赖。

### Piece Tree 加固

- 构造器会拒绝非法 base Piece；
- replacement 校验覆盖负数范围、非法 offset、缺失 Source、换行元数据、Source 区间溢出和文档总长度溢出；
- no-op 替换不会改变 root 或制造 Piece 碎片；
- `Restore` 同时恢复不可变 root 和 Snapshot 捕获的 Source；
- Piece 内部切分会保留 Treap priority，保证堆序不被破坏；
- 测试会检查每个子树缓存的字节数、Piece 数和换行统计；
- Snapshot 隔离覆盖后续编辑、Source 替换、Source 移除和 Restore。

### 本机工具链验证

当前开发环境已使用 MinGW-w64 GCC 和 `CGO_ENABLED=1` 完成验证，可以在 Windows 本机运行 Go race detector。

## 测试现状

当前里程碑包含：

- 26 个普通测试；
- 1 个 Go fuzz target；
- `document/store` 语句覆盖率 100%；
- 基于普通字节切片的随机参考模型测试；
- 10,000 次顺序插入后的平衡性覆盖；
- 编辑期间并发 Snapshot reader；
- 非法范围、整数溢出、短 Source 和错误传播测试。

本机已验证：

```text
go mod verify                                  PASS
go vet ./...                                   PASS
go test ./...                                  PASS
go test -race ./...                            PASS
go test -race -shuffle=on -count=3 ./...       PASS
```

一次 30 秒本机 fuzz 共完成 407,827 次执行，没有发现失败。CI 也会在每次改动时运行短时 fuzz smoke test。

运行主要检查：

```bash
go test ./...
go vet ./...
go test -race ./...
```

运行 Piece Tree fuzz：

```bash
go test ./document/store \
  -run=^$ \
  -fuzz=FuzzTreeMatchesReference \
  -fuzztime=30s
```

Windows race 构建需要 GCC 兼容的 MinGW-w64 工具链，不能直接使用 MSVC-target 的 `cl.exe` 或 `clang-cl.exe`。

## 当前限制

- `ApplyBatch` 还不是真正的原子事务：后面的操作失败时，前面的操作可能已经生效；
- journal 还没有完整的事务批次提交 frame，因此恢复时无法保证多操作事务全有或全无；
- 打开文件时只检查前 64 KiB 是否为合法 UTF-8；
- 文件身份由路径、大小和修改时间组成，不是强内容指纹；
- POSIX 原子替换后还没有同步父目录；
- Session 目录清理和大部分限制仍由宿主负责或硬编码；
- `document/save` 通过 Session 测试被间接覆盖，但缺少独立的故障注入测试；
- 当前没有 release、语义版本承诺或兼容性保证。

## 路线图 / TODO

### P0：事务正确性

- 让 `ApplyBatch` 在内存中保证全有或全无；
- 增加原子 journal batch 格式，例如单个 batch frame 或明确的 begin/commit 记录；
- 恢复时忽略不完整 batch；
- 增加取消和部分写入故障注入测试。

### P1：恢复与持久化

- 对 journal header、frame、payload 长度、CRC 失败和 replay 进行 fuzz；
- 加强基础文件身份检查，并定义兼容与迁移策略；
- 为写入、同步、权限、冲突和替换失败增加独立原子保存测试；
- 审查 POSIX 目录持久性和 Windows 文件替换边界。

### P1：Session 策略与生命周期

- 通过流式方式校验整个打开文件的 UTF-8 合法性；
- 让 undo 配额、插入限制、同步周期和临时路径可配置；
- 明确定义 Session 目录的所有权与清理；
- 改进 undo store 写入错误的传播。

### P2：公共 API

- 决定 `document.Session` 是否作为最终公共 facade；
- 增加包级文档和可运行示例；
- 稳定错误类型和取消语义；
- API 稳定后建立 release 和语义版本。

### P2：可选高层能力

- 通过格式中立接口重新引入结构扫描；
- 基于通用 fragment 构建搜索，而不是重新依赖 Markdown block metadata；
- 渲染和视口虚拟化继续放在宿主/呈现适配层。

## 开发

要求：

- Go 1.26 或更高版本；
- Windows race 构建需要 GCC 兼容的 MinGW-w64 编译器。

克隆并验证：

```bash
git clone https://github.com/moresleep512/docengine.git
cd docengine
go test ./...
```

API 仍在演进。当前阶段尝试使用该模块时应固定到具体 commit，不要假设早期版本之间保持兼容。
