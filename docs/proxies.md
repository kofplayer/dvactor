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
  → RequestProxy.OnMessage: ctx.RequestAsync(真实目标, msg, 0, 回调)
  → 回调中把结果写入 RspChan → system.Request 的调用方拿到响应
```

即"系统外同步请求"被转换为"代理 actor 的异步请求 + channel 回传"。RequestAsync 用 timeout=0 是因为超时控制在调用方 `system.Request` 一侧。

## WatchProxy（[watch_proxy.go](../watch_proxy.go)，ActorType = 12）

解决问题：跨节点 watch/event 时，本机的 watcher（actor 或外部 Queue）与被观察者（watchee）不在同一节点。

机制：

1. **订阅转发**（Router 的 `EnvelopeWatch`/`EnvelopeOuterWatch` 分支，目标非本机时）：不直接跨网，而是在**本机**创建/找到 WatchProxy actor（ActorId = `"<watcheeType>-<watcheeId>"`），把 `InnerWatch`（actor 订阅者）或 `OuterWatch`（外部 Queue 订阅者）发给它。
2. **回源订阅**：WatchProxy 聚合本机对该 watchee 的全部订阅。某 WatchType 从"无订阅者→有"时，`ctx.Watch(watcheeRef, watchType)` 向远程 watchee 发起一次真实 watch（跨网，From 是 WatchProxy 自己，可直接传输），并 `SetStopInterval(0)` 防止代理闲置回收；从"有→无"时 `Unwatch` 并恢复 10 秒回收。
3. **通知回传**：远程 watchee `Notify`/`FireEvent` 时，通知跨网送达本机 WatchProxy，再由它扇出：外部 Queue 直接 `Enqueue`（失败的队列自动退订）；actor watcher 通过 `ctx.LocalRouter` 本地批量投递，From 字段改写为真实 watchee——**对订阅者完全透明，看起来就像 watchee 在本机**。
4. **EventGroup 语义**：跨节点事件要求各节点在 `SystemConfig.ActorTypes` 中声明 `EventHubActorType`，事件按 EventGroup 哈希落到固定节点的 EventHub actor 上（放置规则见模块 L1 文档）。

辅助函数 `GetWatcheeActorRef`（`router.go`）：从 WatchProxy 自己的 ActorId（`"<type>-<id>"`）反解出 watchee 的 ActorRef——**这依赖 ActorId 中不含 `-` 之前的歧义**，是 `SplitN(s, "-", 2)` 约定。

## 已知缺陷

节点断线重连后，WatchProxy 对远程 watchee 的 watch 关系不会自动重建（见 [../todo.md](../todo.md)）。修复方向：重连成功后遍历本机 WatchProxy 重新发起 watch。
