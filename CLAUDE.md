# dvactor — vactor 分布式扩展（L1）

让多个节点上的 actor 系统组成集群协同工作。vactor 的全部特性（Send/Request/Watch/Event）在分布式环境下可用。单机核心机制见 [vactor/CLAUDE.md](../vactor/CLAUDE.md)。

## 集群模型速览

- **接入方式**：通过 vactor 的两个扩展钩子注入——`SetRouter`（集群路由）与 `SetCreateActorRefExFunc`（哈希寻址），见 `system.go` 的 `NewSystem`。
- **Actor 放置**：`CreateActorRef` 未指定 SystemId 时，按 ActorId 哈希（vactor 导出的 `HashActorId`，32 位 FNV-1a，与单机分片共用同一函数）在**声明了该 ActorType 的节点**中选一个放置；`CreateActorRefEx` 指定 SystemId 时直接发往该节点（此时同 type+id 的 actor 可多节点并存）。算法见 `router.go` 的 `CreateActorRefEx`。
- **拓扑静态**：集群拓扑在启动时由 `ClusterConfig` 确定，运行期不可增删节点；无效配置在 `NewSystem` 时直接 panic。
- **序列化**：跨节点消息必须是 protobuf 消息，且需先 `RegisterMessageType(msgType, creator)` 注册；纯本机消息不受限。
- **接入鉴权**：`ClusterConfig.AuthToken` 为共享密钥，非空时注册请求必须携带，防止任意进程冒充节点接入。校验失败时 server 先回带 `ErrorCodeAuthFailed`(108) 的响应再断开，client 据此立即失败重试（而不是白等注册响应超时 10s）。
- **组网**：按 `SystemConfigs` 列表顺序构成全互联（方向规则见 [docs/cluster.md](docs/cluster.md)）；启动阻塞至全部互连。
- **监听地址**：`SystemConfig.Host` 是**其他节点连接本节点**用的地址（client 侧目标）；本机监听网卡由 `ListenHost` 决定，为空表示监听所有网卡（0.0.0.0），可设为 `127.0.0.1` 之类限制暴露面。

## 关键约束与陷阱

- **ActorType 必须 ≥ `dvactor.ActorTypeStart`（= vactor.ActorTypeStart+10 = 20）**，否则 panic；11/12 被内置代理占用（见 [docs/proxies.md](docs/proxies.md)）。
- `SystemConfig.ActorTypes` 决定该类型 actor 的候选放置节点；`EventHubActorType` 若要多节点事件互通，必须在各节点 ActorTypes 中声明。
- 多节点必须**并发**启动：`Start()` 会阻塞等待全互联，单进程内顺序 `Start()` 会互相卡死。
- 节点断线后 client 侧自动退避重连；重连成功后 WatchProxy 自动刷新 watch（分区期间丢失的订阅自愈，机制见 [docs/proxies.md](docs/proxies.md)）。
- `Stop()` 先关集群网络（终止重连、关闭监听与会话）再停 actor 层，幂等、有限时间内返回。
- 跨节点信封的 `Message` 允许为 nil（"只回错误"的响应可正常跨节点）。
- **SystemId 越界（集群版）**：向**未在 `ClusterConfig.SystemConfigs` 中声明**的 SystemId 发消息，`clusterNet.doSend` 返回 `ErrorCodeUnknownSystem`(107)（此前是空指针 panic——在 main/业务 goroutine 触发会直接杀进程）。`CreateActorRefEx` 会无条件按传入的 systemId 建引用，合法性由调用方保证。

## 文件地图

| 文件 | 内容 |
|------|------|
| [system.go](system.go) | `ClusterSystem` 接口、`ClusterConfig`/`SystemConfig`、配置校验、消息类型注册与 proto 编解码、集群停机 |
| [topology.go](topology.go) | 组网角色判定（纯函数）：本节点是否监听、需要主动连接哪些节点 |
| [router.go](router.go) | 集群 `Router`：寻址哈希、本地/远程分流、Watch/OuterWatch/OuterRequest 的代理转发 |
| [cluster_net.go](cluster_net.go) | 集群网络层：envelope ↔ proto 包的双向转换、`Send`/`OnMessage`、组网与连接管理 |
| [cluster_client.go](cluster_client.go) | 主动连接侧：连接、注册握手（带 token 与响应超时）、断线重连循环 |
| [cluster_server.go](cluster_server.go) | 监听侧：注册鉴权、会话绑定、断线清理、同步感知端口绑定结果 |
| [watch_proxy.go](watch_proxy.go) | `WatchProxy`（ActorType=12）：跨节点 watch/event 的本地代理，聚合订阅、回源转发、重连刷新 |
| [request_proxy.go](request_proxy.go) | `RequestProxy`（ActorType=11）：把"系统外发起的跨节点 Request"转为 actor 间 RequestAsync |
| [error.go](error.go) | 分布式错误码 101~108（含 `ErrorCodeUnknownSystem` 107、`ErrorCodeAuthFailed` 108；码表见 [docs/cluster.md](docs/cluster.md)）；本模块自定义码从 `ErrorCodeCustomStart`(200) 起 |
| [protocol/](protocol/) | [cluster.proto](protocol/cluster.proto) 与生成代码；`gen_proto.bat` 重新生成 |
| [engine/](engine/) | 网络基础设施（TCP 传输、帧协议、心跳），细节见 [docs/engine.md](docs/engine.md) |
| [testutil/](testutil/) | 进程内 N 节点集群测试 harness（见下） |

## 示例（[examples/](examples/)）

- [examples/single/main.go](examples/single/main.go)：单节点最小用法（SystemConfig 不带 Host/Port）。
- [examples/multi/](examples/multi/)：双节点集群，`system1`/`system2` 入口共享 [common](examples/multi/common/common.go)，内含 TestSend/TestRequest/TestWatch/TestEvent 四个测试（同时只启用一个，在 common.go 中切换）。

## 测试

`go test ./...` 覆盖消息注册与编解码、放置路由、双/三节点集群集成（send/request/watch/event/重连/鉴权/优雅停机）、engine 帧协议/socket 端到端。集群测试框架在 [testutil/](testutil/cluster.go)：进程内并发启动 N 节点全互联集群（临时端口 + 日志捕获）。测试约定：每个节点的 Register 必须注册该节点收发的**全部**消息类型。

**CI 与依赖版本**：CI 使用的 vactor revision 固定在 [`.vactor-revision`](.vactor-revision)（原先从默认分支拉取，vactor 一提交 CI 结果就可能变）。改完 vactor 并本地跑通后，用 `git -C ../vactor rev-parse HEAD` 更新该文件。手动触发 `workflow_dispatch` 时可用 `vactor_ref` 输入临时覆盖，便于验证配合某个 vactor 分支/PR 是否可行。

> **推送顺序有依赖**：CI 按该 revision 从 GitHub 检出 vactor 源码。若 dvactor 用到了 vactor 的新接口，**必须先提交并推送 vactor → 更新本文件 → 再提交推送 dvactor**。顺序颠倒会让 CI 检出到缺少该接口的旧 vactor 而编译失败；只把本文件更新成"本地 HEAD"也不够——本地 HEAD 未推送时 GitHub 上同样取不到。

## 深入阅读（L2）

- [docs/cluster.md](docs/cluster.md) — 组网、注册握手与鉴权、重连、消息收发路径、错误码
- [docs/protocol.md](docs/protocol.md) — 线协议格式、PkgType、消息序列化规则、心跳参数
- [docs/proxies.md](docs/proxies.md) — WatchProxy / RequestProxy 机制、断线自愈
- [docs/engine.md](docs/engine.md) — engine/net 基础设施、断线回调与心跳机制、回调契约

用户文档：[Readme.md](Readme.md)（EN）· [ReadmeCh.md](ReadmeCh.md)（中文）
