# dvactor — vactor 分布式扩展（L1）

让多个节点上的 actor 系统组成集群协同工作。vactor 的全部特性（Send/Request/Watch/Event）在分布式环境下可用。单机核心机制见 [vactor/CLAUDE.md](../vactor/CLAUDE.md)。

## 集群模型速览

- **接入方式**：通过 vactor 的两个扩展钩子注入——`SetRouter`（集群路由）与 `SetCreateActorRefExFunc`（哈希寻址），见 `system.go` 的 `NewSystem`。
- **Actor 放置**：`CreateActorRef` 未指定 SystemId 时，按 ActorId 哈希在**声明了该 ActorType 的节点**中选一个放置；`CreateActorRefEx` 指定 SystemId 时直接发往该节点（此时同 type+id 的 actor 可多节点并存）。算法见 `router.go` 的 `CreateActorRefEx`。
- **拓扑静态**：集群拓扑在启动时由 `ClusterConfig` 确定，运行期不可增删节点；变更需整体停机重启。
- **序列化**：跨节点消息必须是 protobuf 消息，且需先 `RegisterMessageType(msgType, creator)` 注册；纯本机消息不受限。细节见 L2 协议文档。
- **组网**：按 `SystemConfigs` 列表顺序，序号小的节点监听（server），序号大的主动连接（client），构成全互联。启动时阻塞等待全部节点连上。细节见 L2 集群文档。

## 关键约束与陷阱

- **ActorType 必须 ≥ `dvactor.ActorTypeStart`（= vactor.ActorTypeStart+10 = 20）**，否则 panic；11/12 被内置代理占用（见下）。
- 配置里每个 `SystemConfig.ActorTypes` 决定该类型 actor 的候选放置节点；`EventHubActorType` 若要多节点事件互通，必须在各节点 ActorTypes 中声明。
- `clusterNet.start()` 会**阻塞**直到所有节点互连成功；节点断线后 client 侧每 5 秒自动重连。
- 已知遗留事项见 [todo.md](todo.md)：system 断线重连后 WatchProxy 需要刷新 watch（当前未实现）。

## 文件地图

| 文件 | 内容 |
|------|------|
| [system.go](system.go) | `ClusterSystem` 接口、`ClusterConfig`/`SystemConfig`、消息类型注册与 proto 编解码（4 字节类型前缀） |
| [router.go](router.go) | 集群 `Router`：寻址哈希、本地/远程分流、Watch/OuterWatch/OuterRequest 的代理转发 |
| [cluster_net.go](cluster_net.go) | 集群网络层：envelope ↔ proto 包的双向转换、`Send`/`OnMessage`、连接管理 |
| [cluster_client.go](cluster_client.go) | 主动连接侧：连接、注册握手、断线重连循环 |
| [cluster_server.go](cluster_server.go) | 监听侧：接收注册、会话绑定、断线清理 |
| [watch_proxy.go](watch_proxy.go) | `WatchProxy`（ActorType=12）：跨节点 watch/event 的本地代理，聚合订阅、回源转发 |
| [request_proxy.go](request_proxy.go) | `RequestProxy`（ActorType=11）：把"系统外发起的跨节点 Request"转为 actor 间 RequestAsync |
| [error.go](error.go) | 分布式错误码（101 起）；本模块自定义码从 `dvactor.ErrorCodeCustomStart`(200) 起 |
| [protocol/](protocol/) | [cluster.proto](protocol/cluster.proto) 与生成代码；`gen_proto.bat` 重新生成 |
| [engine/](engine/) | 网络/队列基础设施，细节见 L2 engine 文档 |

## 示例（[examples/](examples/)）

- [examples/single/main.go](examples/single/main.go)：单节点最小用法（SystemConfig 不带 Host/Port）。
- [examples/multi/](examples/multi/)：双节点集群，`system1`/`system2` 入口共享 [common](examples/multi/common/common.go)，内含 TestSend/TestRequest/TestWatch/TestEvent 四个测试（同时只启用一个，在 common.go 中切换）。

## 深入阅读（L2）

- [docs/cluster.md](docs/cluster.md) — 组网、注册握手、重连、消息收发路径
- [docs/protocol.md](docs/protocol.md) — 线协议格式、PkgType、消息序列化规则
- [docs/proxies.md](docs/proxies.md) — WatchProxy / RequestProxy 机制
- [docs/engine.md](docs/engine.md) — engine/net 与 engine/queue 基础设施

用户文档：[Readme.md](Readme.md)（EN）· [ReadmeCh.md](ReadmeCh.md)（中文）
