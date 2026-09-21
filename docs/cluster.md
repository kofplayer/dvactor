# dvactor 集群组网（L2）

> 返回 [dvactor/CLAUDE.md](../CLAUDE.md)。线协议格式见 [protocol.md](protocol.md)。

## 拓扑与连接方向

`ClusterConfig{LocalSystemId, SystemConfigs[]}` 在每个节点上**完全一致**。按 `SystemConfigs` 列表顺序（不是 SystemId 大小）：

- 列表中排在自己**之后**的节点（`weDial=false`）：等待对方连入 → 本节点启动 server 监听自己的 `Port`；
- 排在自己**之前**的节点（`weDial=true`）：本节点作为 client 主动连接对方的 `Host:Port`；
- 列表第一个节点纯 server，最后一个纯 client，中间节点两者兼备 → 全互联。

判断逻辑在 [topology.go](../topology.go) 的 `computeTopology(localIndex, nodeCount)`——**纯函数**，只依赖两个下标即可算出"是否监听 + 需要主动连接哪些节点"，因此多节点形状（首/中/末、单节点、全互联不变量）可以直接单测，不必起网络。`NewClusterNet` 只负责解析出 `localIndex` 再调用它。

> 该字段原名 `passive`，语义与字面相反——被标为 "passive" 的那一方恰恰是本节点主动去连的。现改名 `weDial`（"我们拨号"），`true` = 收发走 `info.cli`，`false` = 收发走 `info.session`。

## 启动与注册握手

`clusterNet.start()`（[cluster_net.go](../cluster_net.go)）：

1. 若本节点不是列表末尾 → 启动 `clusterServer`（[cluster_server.go](../cluster_server.go)）监听本地 Port；
2. 若本节点不是列表开头 → 对每个前序节点启动 `clusterClient`（[cluster_client.go](../cluster_client.go)）；
3. **轮询等待** `connectedSystemCount >= systemCount`（100ms 间隔，仅在进度变化时打日志）后返回 —— 即集群未全员互连前 `Start()` 不会返回。`ClusterConfig.ConnectTimeout` 的取值：**0（默认）= `DefaultConnectTimeout`(30s)**、>0 = 该值、**<0 = 无限等待**（旧行为）。默认值刻意改成有限：`Host` 写错一个字符或某节点根本没起来时，无限等待会让 `Start()` 永久阻塞、既不报错也不返回。超时经 `ClusterStartError()` 暴露给调用方（`Start()` 本身不返回值，只记日志）。监听端口被占等 bind 失败会立即以错误返回，不再被吞进后台日志。

配置在 `NewSystem` 时校验（本地节点必须在列表中、SystemId 不得重复、多节点必须配置端口），无效配置直接 panic。

**注册鉴权**：`ClusterConfig.AuthToken` 非空时，client 注册请求必须携带相同 token。server 校验失败时先回带 `ErrorCodeAuthFailed`(108) 的注册响应、再关闭会话，client 收到该错误码立即判定失败并进入退避重试——避免在没有响应的情况下白等 `registerResponseTimeout`（10s）。token 防止任意进程冒充节点接入，但不提供机密性（明文传输），跨公网部署需配合 TLS 或网络层隔离。

注册握手（client → server）：

```
client: 连接成功 → 发 PkgRegisterSystemReq{SystemId: 本节点ID}
server: 校验 SystemId 存在且 weDial=false（该节点本应由我们主动去连，它反过来注册进来说明两侧方向配置不一致）、未重复注册
        → session 绑定 systemInfo → connectedSystemCount+1 → 回 PkgRegisterSystemRsp{Success}
client: 收到 Rsp → systemInfo.cli 就绪 → connectedSystemCount+1（等待响应带 10s 超时，防止响应丢失永久挂起）
```

注册成功（client 收到 Rsp、server 完成绑定）都会触发 `onSystemReconnected`，驱动本机 WatchProxy 刷新 watch（见 [proxies.md](proxies.md)）。

## 断线与重连

- client 侧：断线回调触发重连循环，**每 5 秒**重试（连接失败/注册失败同样 5 秒退避）。
- server 侧：断线时清理 session 绑定并 `connectedSystemCount-1`。
- 发送时对端不在线返回 `ErrorCodeMessageSendFail`（消息直接失败，无缓冲重投）。
- 链路确实被关闭时（帧不可信、读/写超时、对端进程消失），本侧断线回调必然触发、绑定被同步清理（机制见 [engine.md](engine.md)），对端重连即可恢复，不会出现旧绑定残留导致的永久拒绝。
- `doSend` 在 `info.lock` 临界区内**只取发送器快照**，真正的 socket 写放在锁外：持锁跨写入会让一条慢链路把该系统的重连与绑定清理一起拖住（注册响应的回发同样移出了临界区）。
- 心跳与读超时见 [protocol.md](protocol.md)：半开连接在 `HeartbeatTimeout` 内被检测并进入重连。

## 发送路径

```
envelope → Router.Router（router.go）
  ├─ 目标 SystemId == 本机 → system.LocalRouter（走 vactor 本地流程）
  └─ 远程 → clusterNet.Send(systemId, envelope)
        → envelope 转 proto 包（见 protocol.md 的对照表）
        → doSend：weDial 节点走 cli.SendMessage，否则走 session.SendMessage
```

接收路径：`clusterNet.OnMessage` 按 PkgType 反序列化 → 还原 vactor envelope → `localSystem.LocalRouter` 投入本地调度。**注意：入向消息一律走 LocalRouter，不再经过集群 Router**（目标已是本机）。

### 入向引用的信任边界（必读）

线协议里的 `ActorRef` 由对端填写，而 `SystemId`/`GroupSlot` 在 Go 侧是 `uint16`：

- `ActorRefFromProto` 拒绝 `SystemId`/`GroupSlot` 超出 `uint16` 的引用（否则 **65636 会被静默截断成 100**，
  恰好命中一个真实存在的系统而错投递），也拒绝 `GroupSlot == 0`——框架自建的引用恒为 1..65536，
  0 说明发送方根本没填（proto3 的空 `ActorRef` 子消息就是这个形状）。
- 目标引用还要再过 `clusterNet.localTarget`：**SystemId 必须等于本节点**。少了这一层，一条配置漂移
  或恶意的包会落进 `vactor.group.processEnvelope`，那里的批级 recover 会把**本组当前一整批无关消息**
  一起丢掉（回归测试 `TestOnMessageForeignSystemIdDropsOnlyThatPacket`、`TestOnMessageBatchPoisonIsIsolated`）。
- `FromActorRef` 与被 watch 的 `ActorRef` 允许指向别的环境（响应要发回远端），只有 **To 侧**必须落在本机。

### 单包错误不拆链

`OnMessage` 只在"字节流不可信"时向上返回错误（由 engine 拆包层判定：帧长超过 `MaxPacketSize`）。
**单个信封解不开、消息类型未注册、目标引用非法**都只丢这一条，并记
`cluster packet msgId=N dropped: ...` 日志。把它们当链路级错误的后果是：漏在某个节点注册一个消息类型，
就会让整条集群链路反复断开重连（5s 退避 + 最长 10s 注册窗口），期间**所有类型**的消息都在丢——
滚动升级期间尤其致命。业务处理里的 panic 仍然拆链（该连接状态已不可信），并由
`clusterNet.dispatchSafely` 带堆栈记日志。

停机：`dvactor.System.Stop()` 与 `StopWithContext(ctx)` 都先 `clusterNet.stop()`（终止重连循环并**等其退出**、关闭监听与全部会话），再执行 vactor 的 actor 层停机，幂等且在有限时间内返回。

- 重连循环退出时会主动 `Disconnect()` 当前连接：早先只发一个停止信号，于是 `Stop()` 返回后 TCP 仍然 OPEN、泄漏的读 goroutine 继续回 Pong，对端把这条链路当作健康并持续向一个已停机进程投递。
- 每个重连循环最多等 `clientStopTimeout`(5s)；vactor 层的等待用 `StopWithContext` 加时限（在途 handler 可能阻塞）。
- **必须覆盖 `StopWithContext`**：它来自内嵌的 `vactor.System`，不覆盖就会只停 actor 层、把集群网络留在运行状态（回归测试 `TestClusterStopWithContextAlsoStopsClusterNet`）。

## 错误码（[error.go](../error.go)）

`ErrorCodeMessageCannotSerialize(101)`、`ErrorCodeMessageNotRegister(102)`、`ErrorCodeMessageSerializeFail(103)`、`ErrorCodeMessageLenError(104)`、`ErrorCodeUnknownEnvelope(105)`、`ErrorCodeMessageSendFail(106)`；本模块业务自定义从 `dvactor.ErrorCodeCustomStart(200)` 起（vactor 侧码表见 [vactor/docs/api-reference.md](../../vactor/docs/api-reference.md)）。
