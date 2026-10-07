# 容器层删除中断后的引用安全收敛

> **Workspace:** /Users/ding/Documents/swe/0910061/project-01

---

**User**

节点管理员批量清理已退出容器时，层目录、镜像索引和容器检查点的删除可能在不同阶段中断，重启后会出现仍被镜像引用的层已消失，或孤立层与挂载点始终无法回收。请为这次清理保存可恢复的引用快照和清理意图，按容器、镜像、挂载、层索引和驱动目录的依赖顺序推进；任一步失败或进程中止都保留仍被引用的内容，下一次清理从持久状态继续且不重复删除或复活旧引用。并发删除与镜像创建应收敛到同一层世代，读取不能看到半删除的镜像或层，既有只读存储、overlay 复用、锁定容器和正常崩溃恢复行为继续有效。

**Assistant**

toolName: view_folder

status: success

/Users/ding/Documents/swe/0910061/project-01

这是 containers/storage 项目。我先了解代码结构和现有的删除/检查机制，再规划可恢复清理方案。

toolName: todo_write

status: success

Todos updated: 7 items

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/store.go

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/check.go

toolName: search_by_regex

status: success

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/store.go

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/store.go

现在查看层存储的 `deferredDelete`、元数据持久化方式和锁机制。

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/layers.go

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/layers.go

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/layers.go

toolName: search_by_regex

status: success

toolName: search_by_regex

status: success

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/layers.go

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/containers.go

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/containers.go

查看 Store 接口、启动加载流程、驱动 DeferredRemove 与 tempdir 机制。

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/store.go

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/store.go

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/store.go

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/store.go

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/store.go

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/internal/tempdir/tempdir.go

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/images.go

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cmd/containers-storage/gc.go

toolName: search_by_regex

status: success

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/images.go

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/store.go

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/pkg/ioutils/fswriters.go

toolName: view_files

status: failed

filePath: /Users/ding/Ding/Documents/swe/0910061/project-01/layers.go

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/layers.go

查看层注册点（创建层的位置）和 rwLayerStore 接口，确定世代(generation)挂钩点。

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/layers.go

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/layers.go

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/layers.go

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/jsoniter.go

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/layers.go

研究已充分。查看 `CreateImage`、容器层挂载计数访问和测试辅助后开始实现。

toolName: todo_write

status: success

Todos updated: 7 items

toolName: search_by_regex

status: success

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/store.go

查看测试辅助以编写可在 macOS 上运行的 vfs 测试。

toolName: search_by_regex

status: success

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/store_test.go

查看 `allImageStores`/`allImageStoreKinds` 的组成，确保引用校验覆盖全部镜像存储。

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/store.go

查看 vfs 驱动的 DeferredRemove 实现，确认崩溃后物理目录的回收路径。

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/drivers/vfs/driver.go

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/drivers/overlay/overlay.go

设计完成，开始实现。先在 `layers.go` 中加入持久化层世代（generation fence）。

toolName: todo_write

status: success

Todos updated: 7 items

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/layers.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/layers.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/layers.go

changes: undefined

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/layers.go

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/layers.go

changes: undefined

现在在 `saveFor` 后添加世代持久化方法：

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/layers.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/layers.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/layers.go

changes: undefined

现在让 `Wipe()` 一并清空世代记录：

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/layers.go

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/layers.go

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/layers.go

changes: undefined

`layers.go` 世代机制完成。现在创建核心的 `cleanup.go`：

toolName: Write

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup.go

changes: undefined

接入 Store 接口和启动恢复：

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/store.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/store.go

changes: undefined

先编译检查：

toolName: Shell

status: success

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup.go

changes: undefined

toolName: Shell

status: success

编译通过。优化 `phaseLayerIndex`（层列表外提）并移除死代码：

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup.go

changes: undefined

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup.go

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup.go

changes: undefined

toolName: Shell

status: success

编译与 vet 通过。现在编写测试：

toolName: search_by_regex

status: success

toolName: search_by_regex

status: success

toolName: search_by_regex

status: success

toolName: Write

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup_test.go

changes: undefined

补充测试辅助并修正调用：

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup_test.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup_test.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup_test.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup_test.go

changes: undefined

toolName: Shell

status: success

toolName: search_by_regex

status: success

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup_test.go

changes: undefined

toolName: Shell

status: success

toolName: Shell

status: success

toolName: search_by_regex

status: success

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup_test.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup_test.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup_test.go

changes: undefined

toolName: Shell

status: success

容器创建触发 chroot（macOS 非特权环境限制）。查看 `ContainerOptions` 是否可指定 host mapping：

toolName: search_by_regex

status: success

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/store.go

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup_test.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup_test.go

changes: undefined

toolName: Shell

status: success

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/store.go

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup_test.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup_test.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup_test.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup_test.go

changes: undefined

toolName: Shell

status: success

容器层仍因镜像层带 UIDMap 而走 chroot 拷贝。查看判定逻辑：

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/store.go

toolName: search_by_regex

status: success

测试改用不注入 UID/GID map 的本地 store 辅助（避免 chown 子进程）：

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup_test.go

changes: undefined

toolName: Shell

status: success

toolName: Shell

status: success

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup_test.go

changes: undefined

toolName: Shell

status: success

toolName: Shell

status: success

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup_test.go

changes: undefined

toolName: Shell

status: success

所有新测试通过。运行根包完整测试套件确认无回归：

toolName: todo_write

status: success

Todos updated: 7 items

toolName: Shell

status: success

两个失败都是既有的环境限制（macOS 非特权 chroot）。与基线代码对比验证：

toolName: Shell

status: success

确认是基线已有的环境性失败。跑其余全部测试确认无新增回归：

toolName: Shell

status: success

只剩两个基线环境失败。清理一处死代码并做 Linux 交叉编译验证：

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup.go

changes: undefined

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup.go

changes: undefined

toolName: Shell

status: success

toolName: Shell

status: success

toolName: Shell

status: success

格式化完成。重新构建并完整复审一遍 `cleanup.go`：

toolName: Shell

status: success

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup.go

toolName: view_files

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup.go

复审通过。再跑与层修复相关的 check 测试，并查看最终差异：

toolName: Shell

status: success

补上一个重要细节：只读存储打开时，启动恢复不应创建目录或刷警告（split-store 常见场景）。改为无 journal 目录即静默跳过：

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup.go

changes: undefined

toolName: Shell

status: success

补一个幂等性断言（重复清理不报错、不重复删除）：

toolName: edit_file_search_replace

status: success

filePath: /Users/ding/Documents/swe/0910061/project-01/cleanup_test.go

changes: undefined

toolName: Shell

status: success

toolName: GetDiagnostics

status: success

toolName: Shell

status: success

toolName: todo_write

status: success

Todos updated: 7 items

已完成。在 containers/storage 中实现了**可恢复的批量清理机制**，解决层目录/镜像索引/容器检查点跨阶段中断后的不一致问题。

## 改动文件

**新增 [cleanup.go](file:///Users/ding/Documents/swe/0910061/project-01/cleanup.go)**（核心）
- **持久化引用快照 + 清理意图**：`<graphRoot>/<driver>-cleanup/cleanup.json`，原子（fsync+rename）写入，独立的 `cleanup.lock` 跨进程串行化清理；快照记录全量 容器→镜像/层、镜像→层、层父子 引用图。
- **严格依赖顺序推进**，每条目逐项落盘检查点：
  1. 容器（目录+容器记录，层留待后续）→ 2. 镜像记录（层未删，读者永远看不到镜像指向缺失层）→ 3. 挂载卸载（卸不掉则保留该层）→ 4. 层索引（子层先于父层多趟收敛，复用既有 `deferredDelete`：incomplete 标记→驱动目录改名暂存→索引落盘）→ 5. 驱动目录物理回收（锁外执行；跨进程崩溃由 `GarbageCollect` + tempdir 恢复兜底）。
- **崩溃安全**：每步幂等；任一步失败/进程被杀，已引用内容不动；下次调用或重新打开 store 自动从日志续跑，已完成条目跳过（不重复删除），从不重建已删除记录（不复活旧引用）。
- **层世代 fence**：重新核对当前活跃容器/镜像/子层引用之外，还比对持久化 generation，同 ID 被重新拉取（新世代）时保留新层，绝不删新目录。
- **锁定容器保护**：规划与执行两处检查挂载计数，挂载中的容器整条链保留；支持 DryRun。
- 新 API：`Store.CleanupContainers(CleanupContainersOptions) (CleanupReport, error)`。

**修改 [layers.go](file:///Users/ding/Documents/swe/0910061/project-01/layers.go)**
- 层存储新增持久化 `generations.json` 世代表：层创建发布前 bump 并落盘（`create`、`PutAdditionalLayer`），删除时保留作为墓碑，`Wipe` 重置；`load()` 时随存储一起重载。

**修改 [store.go](file:///Users/ding/Documents/swe/0910061/project-01/store.go)**
- Store 接口暴露 `CleanupContainers`；`GetStore` 完成后尽力自动续跑未完成日志（失败只告警、不影响开库；只读根目录静默跳过）。

**新增 [cleanup_test.go](file:///Users/ding/Documents/swe/0910061/project-01/cleanup_test.go)**：7 个 vfs 测试覆盖全链路清理、共享镜像/层保留、挂载容器保护、DryRun、未知 ID 幂等、崩溃后续跑（容器已删但镜像/孤立层留存）、世代 fence（删除后同 ID 重建，新层与新目录存活、旧暂存目录回收不波及新层）、重复清理幂等。

## 验证
- `go build`（darwin/linux/windows）、`go vet`、`gofmt`、诊断均干净；7 个新测试全部通过。
- 根包全套测试仅 `TestStoreDelete`、`TestStoreMultiList` 失败——已在未改动的基线代码上确认是本机 macOS 非特权环境无法 chroot 所致，与本次改动无关。
- 只读存储（自动恢复不创建任何文件）、overlay/vfs 复用（仍走 `DeferredRemove` 改名暂存）、锁定容器、既有 incompleteFlag/tempdir 崩溃恢复行为均保持不变。

这是本次会话首次完成此类任务，暂不需要沉淀为 skill；若后续多次在本仓库做存储一致性相关改造，可以再让我把这套"持久化日志+世代 fence"模式固化成工作区 skill。
