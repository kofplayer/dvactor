# dvactor 跨节点代理（L2）

> 返回 [dvactor/CLAUDE.md](../CLAUDE.md)。

某些 vactor 语义携带**不可序列化的本机资源**（channel、Queue），无法直接跨节点传输。dvactor 用两个内置代理 actor 在本机把它们转成普通 actor 间消息。两个类型在 `NewSystem` 中自动注册（[system.go](../system.go)）。

## RequestProxy（[request_proxy.go](../request_proxy.go)，ActorType = 11）

解决问题：`system.Request(ref, msg, timeout)` 携带 `RspChan chan *Response`，当 ref 在远程节点时 channel 无法传输。

机制（[router.go](../router.go) 的 `EnvelopeOuterRequest` 分支）：

```
system.Request(远程 ref)
  → Router 发现目标非本机
  → 本地投递给 RequestProxy actor（ActorId = "<目标type>-<目标id>"），消息为 OuterRequest{ToActorRef, Message, RspChan}
  → RequestProxy.OnMessage: ctx.RequestAsync(真实目标, msg, 代理超时, 回调)
  → 回调中把结果写入 RspChan → system.Request 的调用方拿到响应
```

即"系统外同步请求"被转换为"代理 actor 的异步请求 + channel 回传"。代理超时 = 调用方时限 + 5s 余量（保证代理总是晚于调用方超时）；调用方未设时限时退回默认兜底值 `RequestProxyTimeout`(30s)，防止目标永不应答时代理 actor 无法回收。

## WatchProxy（[watch_proxy.go](../watch_proxy.go)，ActorType = 12）

解决问题：跨节点 watch/event 时，本机的 watcher（actor 或外部 Queue）与被观察者（watchee）不在同一节点。

机制：

1. **订阅转发**（Router 的 `EnvelopeWatch`/`EnvelopeOuterWatch` 分支，目标非本机时）：不直接跨网，而是在**本机**创建/找到 WatchProxy actor（ActorId = `"<watcheeType>-<watcheeId>"`），把 `InnerWatch`（actor 订阅者）或 `OuterWatch`（外部 Queue 订阅者）发给它。
2. **回源订阅**：WatchProxy 聚合本机对该 watchee 的全部订阅。某 WatchType 从"无订阅者→有"时，`ctx.Watch(watcheeRef, watchType)` 向远程 watchee 发起一次真实 watch（跨网，From 是 WatchProxy 自己，可直接传输），并 `SetStopInterval(0)` 防止代理闲置回收；从"有→无"时 `Unwatch` 并恢复 10 秒回收。
3. **通知回传**：远程 watchee `Notify`/`FireEvent` 时，通知跨网送达本机 WatchProxy，再由它扇出：外部 Queue 直接 `Enqueue`（失败的队列自动退订）；actor watcher 通过 `ctx.LocalRouter` 本地批量投递，From 字段改写为真实 watchee——**对订阅者完全透明，看起来就像 watchee 在本机**。
4. **EventGroup 语义**：跨节点事件要求各节点在 `SystemConfig.ActorTypes` 中声明 `EventHubActorType`，事件按 EventGroup 哈希落到固定节点的 EventHub actor 上（放置规则见模块 L1 文档）。

> **代理的订阅表（`queuess`/`watcherss`）不做任何过期淘汰，也不该加。** 它与 vactor 侧的 `actorCaches` 同性质——是"谁在通过本代理订阅这个 watchee"的权威状态。按 TTL 或容量清掉会造成**双向静默的订阅丢失**：本机订阅方没收到任何退订、仍以为自己订着，而代理不再向远端回源，通知从此断流且两侧都不报错。代价是内存上界为"本机历史上订阅过的 watcher 数"；watcher actor 消失后条目会残留，这属于**需要先有 watcher 存活通知**才能安全回收的已知限制（vactor 侧的同条契约见 [internals.md](../../vactor/docs/internals.md)"Watcher 缓存与 actor 复活"）。

辅助函数 `GetWatcheeActorRef`（`router.go`）：从 WatchProxy 自己的 ActorId（`"<type>-<id>"`）反解出 watchee 的 ActorRef。实现为 `SplitN(s, "-", 2)`，首段按十进制解析成 ActorType。

> 编解码是一对：`proxyActorId`（`router.go`，`strconv.AppendUint` 手写拼接，不走 `fmt`——它在每次
> watch / 外部请求的路径上）写出 `"<type>-<id>"`，`GetWatcheeActorRef` 用 `SplitN(..., 2)` 读回。
> **watchee 的 ActorId 可以包含 `-`**：类型段是纯数字、永不含 `-`，所以按第一个 `-` 切分总是正确的
> （`51-room-42-player` → type 51 / id `room-42-player`；`1-2-x` 与 `12-x` 也互不撞号）。
> 回归测试 `TestProxyActorIdRoundTrip` 锁住这组互逆关系。
> 解析失败的情形（无 `-`、首段非数字、首段超出 uint32）返回 nil；此时 WatchProxy 会**拒绝登记订阅**
> 并记 Error，而不是把 `nil` 目标交给路由（否则表现为"订阅不报错但通知永远收不到"）。

## 断线重连自愈

本机 WatchProxy 在启动/停止时向 `system.watchProxies` 登记表注册（key=代理引用，value=watchee 引用）。与某系统的连接重新建立后（client 侧注册成功、server 侧收到重新注册都会触发 `onSystemReconnected`），系统向所有以该系统为 watchee 的代理投递 `watchProxyRefresh` 消息，代理对当前仍有订阅者的 WatchType 重新发起 `ctx.Watch`（watchee 侧按 watcher 引用去重，幂等）。由此自愈两类问题：分区期间发出的订阅（watch 信封发送失败被静默丢弃）、对端节点重启丢失的订阅关系。回归测试见 `lifecycle_test.go` 的 `TestClusterWatchSubscriptionSurvivesPartition`。

> 该用例的分区是用**对端节点晚启动**制造的。不要用"往对端发一条它解不开的消息"来模拟分区：
> 单包解码错误已不再拆链（见 [cluster.md](cluster.md)），那条注入手段失效了；真要制造分区应当断链路本身。
