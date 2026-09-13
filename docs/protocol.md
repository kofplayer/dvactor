# dvactor 协议与序列化（L2）

> 返回 [dvactor/CLAUDE.md](../CLAUDE.md)。proto 源文件：[protocol/cluster.proto](../protocol/cluster.proto)（改动后用 `gen_proto.bat` 重新生成 [cluster.pb.go](../protocol/cluster.pb.go)）。

## TCP 线协议（engine/net 层）

```
┌──────────────┬────────────┬─────────────────┐
│ len (4 字节) │ msgId (1B) │ data (len 字节) │
│  大端 uint32 │   uint8    │  proto 序列化   │
└──────────────┴────────────┴─────────────────┘
```

- `len` 只表示 data 长度，总包长 = len + 5。
- 收发两侧在 [engine/net/client/client.go](../engine/net/client/client.go) 与 [engine/net/server/server.go](../engine/net/server/server.go) 中分别做拼包/拆包；接收方循环切片处理粘包。
- **注意**：msgId 只有 1 字节，`PackMessage` 对超过 255 的 msgId 显式报错（当前最大 11，余量充足，但扩协议时需注意）。帧长字段有 `MaxPacketSize`（64MB）上限，超限帧由拆包层报错并断开连接，防止长度字段被伪造导致内存耗尽或溢出 panic。
- **心跳**：引擎层保留 msgId `0xFE`(Ping)/`0xFD`(Pong)，不进入业务 PkgType。双端按 `HeartbeatInterval`（默认 20s）互发 Ping，接收方读超时 `HeartbeatTimeout`（默认 60s）用于检测半开连接。

## PkgType 与信封对照

`clusterNet.Send` / `OnMessage`（[cluster_net.go](../cluster_net.go)）维护 envelope ↔ proto 包的转换：

| PkgType | proto 包 | 对应 vactor envelope |
|---------|----------|---------------------|
| 1 EnvelopeSend | PkgEnvelopeSend | EnvelopeSend |
| 2 EnvelopeBatchSend | PkgEnvelopeBatchSend | EnvelopeBatchSend |
| 3 EnvelopeRequestAsync | PkgEnvelopeRequestAsync | EnvelopeRequestAsync（含 CallbackId/CallbackAddress，均为 uint64） |
| 4 EnvelopeResponseAsync | PkgEnvelopeResponseAsync | EnvelopeResponseAsync |
| 5 EnvelopeRequest | PkgEnvelopeRequest | EnvelopeRequest |
| 6 EnvelopeResponse | PkgEnvelopeResponse | EnvelopeResponse |
| 7 EnvelopeWatch | PkgEnvelopeWatch | EnvelopeWatch |
| 8 EnvelopeNotify | PkgEnvelopeNotify | EnvelopeNotify（拆出 ActorRef/WatchType/Message 三个字段） |
| 9 EnvelopeFireNotify | PkgEnvelopeFireNotify | EnvelopeFireNotify |
| 10 RegisterSystemReq | PkgRegisterSystemReq | 集群注册（含 AuthToken，[握手流程](cluster.md)） |
| 11 RegisterSystemRsp | PkgRegisterSystemRsp | 集群注册 |

> 序号类字段（`RequestId` / `CallbackId` / `CallbackAddress`）在线协议中均为 **uint64**：
> 单调递增的序号若用 32 位，会在约 40 亿次请求后回绕，长跑进程可能因此把新回调与残留条目混淆。

**不可跨节点的信封**：`EnvelopeOuterRequest`、`EnvelopeOuterWatch`（含 channel/队列指针，由 Router 转给本地代理处理，见 [proxies.md](proxies.md)）、以及 vactor 内部的 `envelopeTick`/`envelopeStopedReport`——走 `default` 分支会报 `ErrorCodeUnknownEnvelope`。

## 业务消息序列化

`protocol.Message{Type uint32, Data []byte}` 是业务消息的统一包装：

- **发送**（`system.MarshalMessage`，[system.go](../system.go)）：消息必须实现 `proto.Message` 且已 `RegisterMessageType`；`Data = proto.Marshal(msg)`，消息类型完全由 `Type` 字段承载（历史上 Data 曾带 4 字节大端 msgType 前缀，与 Type 冗余，已移除——同版本集群内两侧一致，无兼容问题）。
- **接收**（`UnmarshalMessage`）：按 `Type` 找 creator 构造空消息，从 `Data` 反序列化。信封中的 `Message` 允许为空（如"只回错误"的响应）：发送侧编码为不含 Message 的包，接收侧还原为 nil。
- `RegisterMessageType(msgType, creator)` 建立 `reflect.Type ↔ msgType` 双向映射；**每个节点都必须注册自己可能收发的全部消息类型**。
- 未注册/非 proto 消息在跨节点发送时直接报错（101/102）；纯本机消息不经序列化，任意 Go 类型均可。

## 响应错误码映射

proto 的 `ErrorCode` 枚举直接 cast 自 `vactor.ErrorCode`（`protocol.ErrorCode(e.Error.Code())`）；接收侧 `vactor.NewVAError(vactor.ErrorCode(pkg.Response.ErrorCode))` 还原。注意 proto 文件里只声明了 0~4 五个值，其余（dvactor 的 101~106、200+）靠强转透传——**改错误码时无需改 proto，但跨语言互操作会看不到枚举名**。
