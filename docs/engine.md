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
- **心跳**：`heartbeatRun` 周期注入 Ping 帧，接收侧在 OnData 回调外拦截 Ping 并回 Pong（不进入业务层）；receiver 对连接设置 `HeartbeatTimeout` 读超时（参数默认值见 [protocol.md](protocol.md)），半开连接检测从 TCP 重传超时（可达 ~15 分钟）缩短到一个超时窗口。心跳参数在连接建立时快照到实例，运行期调整全局变量不影响已有连接。
- `NetSession` 的 `BindObject` 用于把会话绑定到业务对象——clusterServer 用它把 session 绑定到 `systemInfo`（见 [cluster.md](cluster.md)）。

## 回调契约

- `netServer` 对 `SetOnConnect/SetOnMessage/SetOnDisconnect` 均有 nil 防护；`OnMessage` 返回错误会触发该会话断开（client 侧语义一致），上层也可自行提前 `s.Close()`（clusterServer 记日志后关闭）。
- `AcceptorSocket.SetOnListen(f func(error))`：Start 后回调一次监听结果，clusterServer 借此把端口绑定失败同步上抛。
- 替换传输层（如 WebSocket/QUIC）只需实现 `connect` 包的三个接口并在 cluster 层注入；上层拼包逻辑不变。
