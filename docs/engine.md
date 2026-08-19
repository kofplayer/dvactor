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
└── queue/
    ├── def/def.go          Queue 接口（interface{} 元素，非泛型）
    ├── queue.go            NewQueue 工厂（当前唯一实现 = ring）
    └── imp/ring/           环形缓冲实现（ring_buffer.go + queue.go + ringImp.go）
```

## net 层要点

- **拼包/拆包在 client.go 与 server.go 各实现一份**（重复代码）：`len(4,大端) + msgId(1) + data`，接收侧缓冲拼接循环拆包。msgId 发送时被截断为 uint8，注意事项见协议文档。
- `ConnSocket`（[socket/conn.go](../engine/net/connect/socket/conn.go)）：`SendData` 只是入队（engine/queue），sender goroutine 阻塞写出；receiver goroutine 4KB 缓冲循环读并回调 `OnData`。连接关闭通过关闭队列驱动 sender 退出再 `conn.Close()`。TCP KeepAlive 30s（acceptor/connector 均设置）。
- `NetSession` 的 `BindObject` 用于把会话绑定到业务对象——clusterServer 用它把 session 绑定到 `systemInfo`（见 [cluster.md](cluster.md)）。

## queue 层要点

- `queueDef.Queue`（[def/def.go](../engine/queue/def/def.go)）：`Init/Close/IsClose/Enqueue/Dequeue`，元素为 `interface{}`——**与 vactor 的泛型 `Queue[T]` 是两代实现**（vactor 版功能更全：批量、Try 系列、Len）。
- 工厂 `queue.NewQueue(buffLen)` 当前硬编码返回 ring 实现；`Init` 失败直接 panic。
- ring 实现（[imp/ring/](../engine/queue/imp/ring/)）同样是 mutex+cond+环形缓冲，语义与 vactor 版一致（方法名不同：Send/Receive 对应 Enqueue/Dequeue）。

## 改造提示

- 替换传输层（如 WebSocket/QUIC）只需实现 `connect` 包的三个接口并在 cluster 层注入；上层拼包逻辑不变。
- 若统一两代队列实现，优先保留 vactor 的泛型版，engine/socket 目前只用到 `Enqueue/Dequeue/Close/IsClose` 四个方法。
