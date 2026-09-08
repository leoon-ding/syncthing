# Syncthing 上游稳定性更新合入计划（面向 libscm）

## 背景

- `libscm` 通过 `replace github.com/syncthing/syncthing => ../syncthing` 直接绑定当前本地 Syncthing fork，而不是只依赖上游发布版本。
- `libscm` 当前依赖了 fork 中额外暴露的 `Internals` 能力，而不只是上游默认公开 API。核心依赖面在 [service/syncthing/shared.go](../../libscm/service/syncthing/shared.go) 和 [service/syncthing/wrapper.go](../../libscm/service/syncthing/wrapper.go)。
- 因此，这次目标不应是“整体追到 `v2.0.16`”，而应是“按稳定性价值分批 cherry-pick 上游提交，同时保护现有 fork 扩展面和 `libscm` 行为”。

## libscm 当前对 Syncthing fork 的直接依赖

- `WrapperPort` 已把多个 fork 扩展能力向上暴露，例如 `GetFolderErrors`、`GetLocalFileInfo`、`GetGlobalFileInfos`、`GetApplePHAssetGlobalInfos`、`ScanFolder`、`Override`、`Revert`、`ResetFolder`。[service/syncthing/shared.go:78](../../libscm/service/syncthing/shared.go)
- 上述能力最终都走 `s.app.Internals.*`，例如：
  - `FolderErrors`、`Completion`、`LocalChangedFolderFiles`。[service/syncthing/wrapper.go:905](../../libscm/service/syncthing/wrapper.go) [service/syncthing/wrapper.go:921](../../libscm/service/syncthing/wrapper.go) [service/syncthing/wrapper.go:945](../../libscm/service/syncthing/wrapper.go)
  - `LoadIgnores`、`LocalFileInfo`、`DBSnapshot`、`RemoteNeedFolderFiles`。[service/syncthing/wrapper.go:992](../../libscm/service/syncthing/wrapper.go) [service/syncthing/wrapper.go:1027](../../libscm/service/syncthing/wrapper.go) [service/syncthing/wrapper.go:1063](../../libscm/service/syncthing/wrapper.go) [service/syncthing/wrapper.go:1147](../../libscm/service/syncthing/wrapper.go)
  - `ScanFolder`、`Override`、`Revert`、`ResetFolder`。[service/syncthing/wrapper.go:1168](../../libscm/service/syncthing/wrapper.go) [service/syncthing/wrapper.go:1179](../../libscm/service/syncthing/wrapper.go) [service/syncthing/wrapper.go:1190](../../libscm/service/syncthing/wrapper.go) [service/syncthing/wrapper.go:1229](../../libscm/service/syncthing/wrapper.go)
- `filesync` 引擎把这些能力用于自动恢复与状态判断：
  - 通过 `GetFolderErrors` + `GetLocalCompletion` 生成主动 summary。[service/filesync/engine.go:305](../../libscm/service/filesync/engine.go)
  - 在 send-only / receive-only 目录异常时触发 `Override` / `Revert`。[service/filesync/engine.go:468](../../libscm/service/filesync/engine.go)
  - 在目录丢失时执行 `ResetFolder` 或 `RemoveFolder` 以快速恢复。[service/filesync/engine.go:582](../../libscm/service/filesync/engine.go)
  - 运行态把 `idle 但未 100%` 的情况用 `FolderErrors` 转成用户可见的 error 状态。[service/filesync/runtime.go:324](../../libscm/service/filesync/runtime.go)

## 关键判断

- 当前推荐的是“按提交 cherry-pick”，不是“直接 merge/rebase 到 `v2.0.16`”。
- 原因不是 merge 一定会失败，而是完整抬升会顺带带入更宽的 `go.mod` / 依赖 / 运行时变化，而本次真正高价值的稳定性修复只集中在少数几个提交。
- `libscm` 当前 `go.mod` 是 `go 1.25.7`，已经高于 `v2.0.16` 的 `go 1.25.0`，因此本计划内的 cherry-pick 不会因为语言版本本身阻塞 [go.mod](../../libscm/go.mod)。

## 逐提交冲突与影响分析

### 1. `fd129825b`

- 标题：`fix(protocol): verify compressed message length before decompression`
- 价值：协议层边界检查，防止异常压缩消息在解压前就进入错误路径，优先级最高。
- 与 `libscm` 的直接冲突：
  - 无接口冲突。
  - `libscm` 没有自己构造 Syncthing BEP 压缩消息，也没有绕过 Syncthing 正常收包路径。
- 对 `libscm` 的影响：
  - 纯正向影响，主要体现在嵌入式场景下更不容易因异常 peer 或坏包进入异常状态。
  - 对 `filesync` 的恢复、状态计算、Wrapper 接口无行为兼容性风险。
- 建议：单独 cherry-pick，作为第一步。

### 2. `9ffce6e3f`

- 标题：`chore(sqlite): reduce max open connections, keep them open permanently`
- 价值：减少 SQLite 连接 churn，把 `MaxOpenConns` 从 `16` 降到 `6`，同时设置 `MaxIdleConns`，有利于长驻服务稳定性。
- 与 `libscm` 的直接冲突：
  - 无接口冲突。
  - 不会影响 `WrapperPort`、`Internals` 扩展面。
- 对 `libscm` 的影响：
  - `libscm` 的 `filesync`、`runtime`、`engine` 依赖大量索引/状态查询，减少 DB 连接抖动通常更有利于稳定恢复和长期运行。
  - 这项变化更可能影响性能轮廓，而不是业务语义。
- 风险：
  - 理论上大规模并发索引或查询场景下，连接数降低可能改变吞吐表现。
  - 但对 `libscm` 当前以单实例嵌入式使用为主的模式，风险低于收益。
- 建议：与 `fd129825b` 同批合入。

### 3. `75dd94012`

- 标题：`chore(config, connections): use same reconnection interval for QUIC and TCP`
- 价值：统一 QUIC / TCP 重连节奏，并把默认 `ReconnectIntervalS` 从 `60` 调整到 `20` 秒，符合 `libscm` “更快恢复 syncthing 异常状态”的目标。
- 与 `libscm` 的直接冲突：
  - 无直接接口冲突。
  - 该提交改的是 `lib/config` migration 和 `lib/connections/quic_dial.go`，不会覆盖当前 fork 在 `lib/connections/tcp_listen.go` / `lib/connections/quic_listen.go` 的 accept backoff 定制。
- 对 `libscm` 的影响：
  - 恢复节奏会更积极，可能更快触发连接恢复，进而更快推进 `filesync` 的状态回流。
  - `Engine` 中依赖 folder summary / completion 的自动恢复流程，理论上会更早看到恢复后的状态变化。[service/filesync/engine.go:468](../../libscm/service/filesync/engine.go)
- 风险：
  - 更频繁重连可能提高弱网下的短时连接噪音。
  - 老配置会经过 migration 收敛为新语义，需要验证现有 data dir 上的配置升级结果。
- 建议：作为第一批第三个提交，合后重点做“断网/重连/重启”恢复验证。

### 4. `5cf9168dc`

- 标题：`chore(db): add ability to wait for programmatically started database maintenance, query last maintenance time`
- 价值：允许程序触发 DB maintenance 后等待完成，并查询上次维护时间。
- 与 `libscm` 的直接冲突：
  - 当前 `libscm` 没有调用 `StartMaintenance()` 或 `LastMaintenanceTime()`，所以不存在上层编译错误风险。
  - 但该提交会修改 fork 自定义过的 [lib/syncthing/syncthing.go](../lib/syncthing/syncthing.go)，这里是本次最明确的手工冲突点。
- 预计冲突点：
  - 当前 fork 的 `StartMaintenance()` 是无返回值；上游提交会改成返回 `<-chan error`。
  - 当前 fork 在 `setupGUI()` 里提前启动了 `summaryService`，该文件已经偏离上游，所以不能盲目 `cherry-pick -m` 后相信自动合并结果。
- 对 `libscm` 的影响：
  - 短期：无必须联动改动，因为 `libscm` 当前没用这个接口。
  - 中期：这是一个可选增强点，未来如果 `libscm` 想把“索引维护中”或“上次 DB 维护时间”暴露给上层，可以直接复用。
- 建议：
  - 保留上游新能力，但当前阶段不要顺手在 `libscm` 里新增调用点。
  - 只做最小兼容合并，避免把本次任务扩成状态页/接口扩展任务。

### 5. `5febc056a`

- 标题：`fix(protocol): limit size of incoming request messages`
- 价值：限制单个 incoming request 的 size，继续加固协议边界。
- 与 `libscm` 的直接冲突：
  - 无接口冲突。
  - `libscm` 没有自定义构造超大 Syncthing request 的代码路径；当前可见的封装是通过 `Internals` 做查询和索引访问，而不是重写 BEP request 生成逻辑。
- 对 `libscm` 的影响：
  - 纯正向，防止异常请求拖垮进程或造成异常资源消耗。
  - 对正常文件同步、索引查询、Apple 资源聚合逻辑无兼容风险。
- 风险：
  - 如果存在非标准 peer 发送异常 size 的 request，会变成更早失败。这是期望行为。
- 建议：第一批稳定后，立即并入第二批开头。

### 6. `f538b4707`

- 标题：`chore(model): slightly improve handling of pulling empty blocks`
- 价值：优化全零块 corner case，减少无意义网络拉取和写盘。
- 与 `libscm` 的直接冲突：
  - 无直接接口冲突。
  - 改动位于 `lib/model/folder_sendrecv.go`，不会碰 `libscm` 的 wrapper 扩展面。
- 对 `libscm` 的影响：
  - 对大文件、稀疏文件、备份目录更有价值，理论上可减少一些“看起来在同步，实际在搬零块”的低效行为。
  - 对 `filesync` 用户可见状态主要影响在耗时和 I/O，不在业务语义。
- 风险：
  - 需要跑一次包含零块/稀疏文件的同步验证，避免 corner case 变成“同步完成但文件内容不对”。
- 建议：放在协议修复之后。

### 7. `2721b7b52`

- 标题：`chore(model): more efficient tracking of renames during scan`
- 价值：扫描期 rename 跟踪更高效，适合大目录与照片/文档库。
- 与 `libscm` 的直接冲突：
  - 无直接接口冲突。
  - 但这是扫描核心路径变更，行为面比前几个提交更深。
- 对 `libscm` 的影响：
  - `libscm` 有主动 `ScanFolder()` 和大量依赖扫描后状态变化的逻辑。[service/filesync/support.go](../../libscm/service/filesync/support.go) [service/filesync/engine.go:560](../../libscm/service/filesync/engine.go)
  - 如果 rename 识别策略变化，可能影响 folder summary、completion 推进速度、`item finished` 事件节奏，进而影响自动恢复判断。
- 风险：
  - 需要专门验证 rename-heavy 场景，尤其是大批量移动照片、目录改名、跨层级 rename。
- 建议：第二批最后一个，必须单独验证。

## 本次不建议纳入的上游提交

### `b39c56f82`

- 原因：它移除了 inode change time 跟踪，会改变扫描/变更检测语义。
- 对 `libscm` 而言，这不是“低风险稳定性修复”，而是“需要单独验语义”的行为变化。
- 建议：从当前计划中排除，后续单开一轮评估。

### `86ac4e501`

- 原因：它引入 `FullBlockIndex` 配置能力，更偏资源/性能策略，不是本轮稳定性最小集合。
- 建议：后续如果 `libscm` 有超大目录或 DB 膨胀问题，再单独评估。

## 详细合入计划

### Phase 0：建基线与保护现场

- 在 Syncthing fork 新建工作分支，例如 `codex/libscm-stability-upstream-batch1`。
- 记录当前 fork 相对 `v2.0.14` 的自定义改动，重点复核：
  - [lib/syncthing/internals.go](../lib/syncthing/internals.go)
  - [lib/syncthing/syncthing.go](../lib/syncthing/syncthing.go)
  - [lib/connections/tcp_listen.go](../lib/connections/tcp_listen.go)
  - [lib/connections/quic_listen.go](../lib/connections/quic_listen.go)
  - [lib/fs/filesystem.go](../lib/fs/filesystem.go)
- 在 `libscm` 侧确认 `go test` 基线可过，避免把已有问题误判为合入回归。

### Phase 1：第一批低风险高收益提交

- 顺序：
  1. `git cherry-pick fd129825b`
  2. `git cherry-pick 9ffce6e3f`
  3. `git cherry-pick 75dd94012`
  4. `git cherry-pick 5cf9168dc`
- 处理原则：
  - 前三个提交若无冲突，直接保留上游实现。
  - `5cf9168dc` 出现冲突时，以“保留现有 fork 定制 + 引入上游新接口语义”为准。
  - 不在这一阶段顺手重构 `libscm` 对 maintenance 的调用，因为当前没有调用点。

### Phase 2：第一轮验证

- 在 `syncthing` repo：
  - 跑与 `protocol`、`db/sqlite`、`config`、`connections` 相关的定向测试。
  - 如果时间允许，再跑覆盖面更大的 Syncthing 核心测试集。
- 在 `libscm` repo：
  - 跑 `service/syncthing/...`
  - 跑 `service/filesync/...`
  - 跑 `cmd/bifrost/...`
- 手工验证场景：
  - 启动 `libscm` 后建立 send-only / receive-only / send-receive 目录。
  - 断网再恢复，观察重连时间是否变短，且没有异常重连风暴。
  - 制造目录丢失，确认 `ResetFolder()` 逻辑仍能恢复。[service/filesync/engine.go:582](../../libscm/service/filesync/engine.go)
  - 观察 `idle but not 100%` 时 `FolderErrors` 是否仍能正确反映为用户可见错误。[service/filesync/runtime.go:324](../../libscm/service/filesync/runtime.go)

### Phase 3：第二批协议与模型补强

- 顺序：
  1. `git cherry-pick 5febc056a`
  2. `git cherry-pick f538b4707`
  3. `git cherry-pick 2721b7b52`
- 处理原则：
  - `5febc056a` 预期无冲突，直接合。
  - `f538b4707` 与 `2721b7b52` 都是 model 层行为优化，需要每个提交后各跑一轮定向验证，不建议三个一起压栈后再一次性测。

### Phase 4：第二轮验证

- 定向回归：
  - 大文件同步。
  - 稀疏文件或包含大量零块的文件同步。
  - 大批量 rename / move 场景。
  - Apple 资源目录和普通目录混合场景，确认 `GetApplePHAssetGlobalInfos()` 结果未受扫描/模型改动破坏。[service/syncthing/wrapper.go:1091](../../libscm/service/syncthing/wrapper.go)
- 重点观察：
  - completion 百分比推进是否正常。
  - `FolderSummary` / `StateChanged` 事件驱动的自动恢复逻辑是否仍按预期触发。[service/filesync/engine.go:468](../../libscm/service/filesync/engine.go)
  - version archive reconcile 是否仍只在 idle 且双端不欠账时运行。[service/filesync/engine.go:360](../../libscm/service/filesync/engine.go)

## 建议的提交粒度

- 不建议把全部提交 squash 成一个“upgrade syncthing”大提交。
- 建议保留至少两个逻辑提交：
  - 提交 1：协议/DB/重连稳定性批次。
  - 提交 2：model 层补强批次。
- 如果 `5cf9168dc` 的手工冲突处理较多，可以单独形成一个 commit，便于后续 bisect。

## 建议的回退策略

- 如果 Phase 1 之后出现回归，优先怀疑：
  - `75dd94012` 的重连节奏变化。
  - `5cf9168dc` 的 `lib/syncthing/syncthing.go` 手工冲突处理。
- 如果 Phase 3 之后出现回归，优先怀疑：
  - `2721b7b52` 的 rename tracking 行为变化。
  - `f538b4707` 的零块处理路径。
- 因为计划是分批 cherry-pick，所以可以按批次回退，而不需要回滚整个升级面。

## 最终建议

- 先做最小稳定性集，不要一次性追完整 `v2.0.16`。
- 第一批真正值得马上合的是：
  - `fd129825b`
  - `9ffce6e3f`
  - `75dd94012`
  - `5cf9168dc`
- 第一批验证稳定后，再补：
  - `5febc056a`
  - `f538b4707`
  - `2721b7b52`
- 当前不建议纳入：
  - `b39c56f82`
  - `86ac4e501`
