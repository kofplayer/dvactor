# dvactor engine 基础设施（L2）

> 返回 [dvactor/CLAUDE.md](../CLAUDE.md)。`engine/` 是与 actor 无关的网络/队列基础库，被 clusterNet 使用。线协议格式见 [protocol.md](protocol.md)。

## 结构

```
engine/
├── net/
│   ├── connect/            传输抽象接口
│   │   ├── conn.go           Conn：SendData/Disconnect/RemoteAddr + 回调
│   │   ├── acceptor.go       Acceptor：Start/Stop/SetOnAccept
│   │   ├── connector.go      Connector：Conn + Connect/SetOnConnect
│   │   ├── packet.go         帧协议：PacketSplitter 累积拆帧 + 最大帧长校验
│   │   ├── heartbeat.go      心跳：Ping/Pong 帧 msgId（0xFE/0xFD，编译期常量）、间隔与读超时默认值
│   │   ├── config.go         发送/连接上界：WriteTimeout、SendQueueMaxFrames、DialTimeout
│   │   └── socket/           TCP 实现（socketNetConnect 包）
│   ├── client/client.go    NetClient：Connector + 拼包/拆包 + 回调
│   ├── server/server.go    NetServer：Acceptor + session 管理 + 拼包/拆包
│   └── session/            NetSession（绑定 Conn + SendMessageFunc + BindObject）
│                          与 SessionMgr（id 自增、map + RWMutex）
└── （无队列目录：发送队列直接复用 vactor 的泛型 Queue[[]byte]）
```

## net 层要点

- **拼包/拆包在 client.go 与 server.go 各实现一份**（重复代码）：帧格式与长度上限见 [protocol.md](protocol.md)；`PacketSplitter.Next` 对超限帧返回错误，调用方断开连接。
- `ConnSocket`（[socket/conn.go](../engine/net/connect/socket/conn.go)）：`SendData` 只是入队（`vactor.Queue[[]byte]`，无界、关闭后返回 false），sender goroutine 阻塞写出；receiver goroutine 4KB 缓冲循环读并回调 `OnData`。连接关闭通过关闭队列驱动 sender 退出再 `conn.Close()`。TCP KeepAlive 30s（acceptor/connector 均设置）。
- **断线回调幂等且必达**（`notifyDisconnect` + `sync.Once`）：读错误、写错误、本端主动 Disconnect 三条路径都恰好触发一次 `OnDisconnect`——此前"队列已关闭"路径会吞掉回调，导致 server 主动关会话后绑定永不清理、对端重连被永久拒绝（P0，已修复）。
- **Disconnect 一定关掉 fd**：早先它只做 `q.Close()`，把关 fd 交给 senderRun"排空队列之后"那一步；而对端停止读取时 senderRun 会永久卡在 `conn.Write` 上，于是 fd 不关、回调不触发、绑定状态不清，**网络分区无法自愈**。现在 `Disconnect` 设一个 `DisconnectGrace`(100ms) 写超时并挂同长度的兜底定时器（停机时可能丢少量积压，换来有界且可自愈）；`senderRun` 另有 `WriteTimeout`(30s) 每帧写超时，用来识别真正的卡死。
- **发送队列有界**：`SendQueueMaxFrames`(默认 8192 帧)。无界队列在慢对端下会把本端内存吃到 OOM，而 `SendData` 一路返回"成功"、上层据此误判已送达，心跳帧同队列所以链路还一直显示健康；队满现在会以 `ErrSendQueueFull` 在 `SendData` 上报错，上层得以降级。
- **读回调的 panic 不会击穿进程**：`receiverRun` 经 `safeOnData` 调用上层，panic 被转成一个连接错误（走完整的失败路径收尾）。集群层还额外用 `clusterNet.dispatchSafely` 把 panic 连同堆栈打进日志再返回错误——engine 层没有日志设施，只靠它兜底会丢掉现场。`n>0` 且同时返回错误的那次 `Read` 会先交付数据再判错，"发完即关"的最后一帧不会丢。
- **会话数上限**：`netServer.SetMaxSessions(n)` + `SetOnReject`。accept 即登记会话，未注册连接要到 `HeartbeatTimeout`(60s) 才回收，没有上限时端口扫描可让 session 表与 fd 无界增长。dvactor 按集群规模取 `4×节点数`。
- **会话字段有锁**：`netSession` 的 bindObject/conn/sendMessageFunc 会被 receiver、sender（写失败也走断线回调）与任意 Close/SendMessage 调用方并发读写，全部字段访问走 RWMutex，且**不持锁跨 socket 写入**。
- **拨号有超时**：`ConnectorSocket.Connect` 用 `net.DialTimeout`(`DialTimeout` 默认 10s)。黑洞地址下原来的 `net.Dial` 会按内核 SYN 重试挂上数十秒到分钟级，期间重连循环停在 `Connect()` 上、`Stop()` 也打不断。
- **心跳**：`heartbeatRun` 周期注入 Ping 帧，接收侧在 OnData 回调外拦截 Ping 并回 Pong（不进入业务层）；receiver 对连接设置 `HeartbeatTimeout` 读超时（参数默认值见 [protocol.md](protocol.md)），半开连接检测从 TCP 重传超时（可达 ~15 分钟）缩短到一个超时窗口。心跳参数在连接建立时快照到实例，运行期调整全局变量不影响已有连接。
- `NetSession` 的 `BindObject` 用于把会话绑定到业务对象——clusterServer 用它把 session 绑定到 `systemInfo`（见 [cluster.md](cluster.md)）。

## 回调契约

- `netServer` 对 `SetOnConnect/SetOnMessage/SetOnDisconnect` 均有 nil 防护；acceptor 的 `onAcceptFunc` 未设置时直接关闭新连接（不 panic、不泄漏 fd）。`OnMessage` 返回错误会触发该会话断开（client 侧语义一致），上层也可自行提前 `s.Close()`（clusterServer 记日志后关闭）。**因此上层必须区分错误等级**：只有"字节流不可信"才该返回错误，单包业务错误应记日志后返回 nil——否则一条不兼容消息就能拆掉整条集群链路（见 [cluster.md](cluster.md)）。
- `AcceptorSocket.SetOnListen(f func(error))`：Start 后回调一次监听结果，clusterServer 借此把端口绑定失败同步上抛。
- 替换传输层（如 WebSocket/QUIC）只需实现 `connect` 包的三个接口并在 cluster 层注入；上层拼包逻辑不变。
