# dvactor 集群组网（L2）

> 返回 [dvactor/CLAUDE.md](../CLAUDE.md)。线协议格式见 [protocol.md](protocol.md)。

## 拓扑与连接方向

`ClusterConfig{LocalSystemId, SystemConfigs[]}` 在每个节点上**完全一致**。按 `SystemConfigs` 列表顺序（不是 SystemId 大小）：

- 列表中排在自己**之后**的节点（`passive=false`）：等待对方连入 → 本节点启动 server 监听自己的 `Port`；
- 排在自己**之前**的节点（`passive=true`）：本节点作为 client 主动连接对方的 `Host:Port`；
- 列表第一个节点纯 server，最后一个纯 client，中间节点两者兼备 → 全互联。

判断逻辑在 `NewClusterNet`（`passive: !findSelf`，即列表中位于自己之前的节点标记为 passive）。

## 启动与注册握手

`clusterNet.start()`（[cluster_net.go](../cluster_net.go)）：

1. 若本节点不是列表末尾 → 启动 `clusterServer`（[cluster_server.go](../cluster_server.go)）监听本地 Port；
2. 若本节点不是列表开头 → 对每个前序节点启动 `clusterClient`（[cluster_client.go](../cluster_client.go)）；
3. **阻塞轮询** `connectedSystemCount >= systemCount`（每 3 秒打印等待日志）后返回 —— 即集群未全员互连前 `Start()` 不会返回。

注册握手（client → server）：

```
client: 连接成功 → 发 PkgRegisterSystemReq{SystemId: 本节点ID}
server: 校验 SystemId 存在且非 passive（防止方向反了）、未重复注册
        → session 绑定 systemInfo → connectedSystemCount+1 → 回 PkgRegisterSystemRsp{Success}
client: 收到 Rsp → systemInfo.cli 就绪 → connectedSystemCount+1
```

## 断线与重连

- client 侧：断线回调触发重连循环，**每 5 秒**重试（连接失败/注册失败同样 5 秒退避）。
- server 侧：断线时清理 session 绑定并 `connectedSystemCount-1`。
- 发送时对端不在线返回 `ErrorCodeMessageSendFail`（消息直接失败，无缓冲重投）。
- 已知缺陷：重连后 WatchProxy 的 watch 关系不会自动恢复，见 [../todo.md](../todo.md)。

## 发送路径

```
envelope → Router.Router（router.go）
  ├─ 目标 SystemId == 本机 → system.LocalRouter（走 vactor 本地流程）
  └─ 远程 → clusterNet.Send(systemId, envelope)
        → envelope 转 proto 包（见 protocol.md 的对照表）
        → doSend：passive 节点走 cli.SendMessage，否则走 session.SendMessage
```

接收路径：`clusterNet.OnMessage` 按 PkgType 反序列化 → 还原 vactor envelope → `localSystem.LocalRouter` 投入本地调度。**注意：入向消息一律走 LocalRouter，不再经过集群 Router**（目标已是本机）。

## 错误码（[error.go](../error.go)）

`ErrorCodeMessageCannotSerialize(101)`、`ErrorCodeMessageNotRegister(102)`、`ErrorCodeMessageSerializeFail(103)`、`ErrorCodeMessageLenError(104)`、`ErrorCodeUnknownEnvelope(105)`、`ErrorCodeMessageSendFail(106)`；本模块业务自定义从 `dvactor.ErrorCodeCustomStart(200)` 起（vactor 侧码表见 [vactor/docs/api-reference.md](../../vactor/docs/api-reference.md)）。
