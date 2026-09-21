package dvactor

import (
	"fmt"
	"math"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	netClient "github.com/kofplayer/dvactor/engine/net/client"
	netSession "github.com/kofplayer/dvactor/engine/net/session"
	"github.com/kofplayer/dvactor/protocol"
	"github.com/kofplayer/vactor"
	"google.golang.org/protobuf/proto"
)

func NewClusterNet(localSystem *system, clusterConfig *ClusterConfig) *clusterNet {
	localSystemIndex := -1
	for i, config := range clusterConfig.SystemConfigs {
		if clusterConfig.LocalSystemId == config.SystemId {
			localSystemIndex = i
			break
		}
	}
	// validateClusterConfig 已保证本地节点一定在列表中
	topology := computeTopology(localSystemIndex, len(clusterConfig.SystemConfigs))

	systemInfos := make(map[vactor.SystemId]*systemInfo, len(clusterConfig.SystemConfigs))
	for i, config := range clusterConfig.SystemConfigs {
		systemInfos[config.SystemId] = &systemInfo{
			config: config,
			weDial: topology.weDial(i),
		}
	}
	return &clusterNet{
		localSystem:   localSystem,
		topology:      topology,
		clusterConfig: clusterConfig,
		systemInfos:   systemInfos,
		systemCount:   len(systemInfos),
	}
}

func ActorRefToProto(actorRef vactor.ActorRef) *protocol.ActorRef {
	if actorRef == nil {
		return nil
	}
	ref, isImpl := actorRef.(*vactor.ActorRefImpl)
	if !isImpl {
		// 非框架自建的 ActorRef 无法序列化：返回 nil，接收侧按"目标缺失"丢弃
		return nil
	}
	return &protocol.ActorRef{
		SystemId:  uint32(ref.SystemId),
		GroupSlot: uint32(ref.GroupSlot),
		ActorType: uint32(ref.ActorType),
		ActorId:   string(ref.ActorId),
	}
}

// ActorRefFromProto 把线协议引用还原为框架引用。返回 nil 表示"按目标缺失处理"：
// 包里没有这个引用，或字段非法（见下）。
//
// 线协议用 uint32 承载 SystemId/GroupSlot 是为了将来能放宽 Go 侧类型，但当前
// vactor.SystemId/GroupSlot 都是 uint16：**静默截断会把 65636 变成 100**，恰好命中
// 一个真实存在的系统而错投递。因此越界一律判为畸形引用并丢弃。
// GroupSlot==0 同样非法——框架自建的引用恒为 1..65536，0 意味着发送方根本没填
// （proto3 的空 ActorRef 子消息就是这个形状）。
func ActorRefFromProto(actorRef *protocol.ActorRef) vactor.ActorRef {
	if actorRef == nil {
		return nil
	}
	if actorRef.SystemId > math.MaxUint16 || actorRef.GroupSlot > math.MaxUint16 || actorRef.GroupSlot == 0 {
		return nil
	}
	return &vactor.ActorRefImpl{
		SystemId:  vactor.SystemId(actorRef.SystemId),
		GroupSlot: vactor.GroupSlot(actorRef.GroupSlot),
		ActorType: vactor.ActorType(actorRef.ActorType),
		ActorId:   vactor.ActorId(actorRef.ActorId),
	}
}

// localTarget 校验"必须由本节点亲自投递"的目标引用（ToActorRef / ToActorRefs 元素）。
//
// 这一层校验是必需的：vactor 的 group 在处理信封时会检查 SystemId，但它是按"本地
// 编程错误"来处理这类异常的。远端字节流给出的引用不受该假设约束——若不加校验，
// 一个 SystemId 与配置漂移的对端就能让本组当前一整批消息被丢弃。
// 校验失败时只丢这一条（返回 nil，由 LocalRouter 记日志并丢弃），**不返回 error**，
// 否则一条畸形包就会拆掉整条集群链路。
func (cn *clusterNet) localTarget(actorRef *protocol.ActorRef) vactor.ActorRef {
	ref := ActorRefFromProto(actorRef)
	if ref == nil {
		if actorRef != nil {
			cn.localSystem.LogError("dropping packet: target actor ref from system %v is invalid (slot %v, type %v, id %q)",
				actorRef.SystemId, actorRef.GroupSlot, actorRef.ActorType, actorRef.ActorId)
		}
		return nil
	}
	if local := cn.clusterConfig.LocalSystemId; ref.GetSystemId() != local {
		cn.localSystem.LogError("dropping packet: target system %v != local system %v (cluster configs disagree?), actor %v/%q",
			ref.GetSystemId(), local, ref.GetActorType(), ref.GetActorId())
		return nil
	}
	return ref
}

// localTargets 是 localTarget 的批量版本：滤掉非法收件人，全部非法时返回空切片。
func (cn *clusterNet) localTargets(actorRefs []*protocol.ActorRef) []vactor.ActorRef {
	refs := make([]vactor.ActorRef, 0, len(actorRefs))
	for _, r := range actorRefs {
		if ref := cn.localTarget(r); ref != nil {
			refs = append(refs, ref)
		}
	}
	return refs
}

// marshalMessage 包装 MarshalMessage：nil 消息（如"只回错误"的响应）允许跨节点，
// 编码为不含 Message 的包；接收侧 unmarshalMessage 对称还原为 nil。
func (cn *clusterNet) marshalMessage(msg interface{}) (*protocol.Message, vactor.VAError) {
	if msg == nil {
		return nil, nil
	}
	return cn.localSystem.MarshalMessage(msg)
}

// unmarshalMessage 包装 UnmarshalMessage：包内未携带 Message 时返回 (nil, nil)。
func (cn *clusterNet) unmarshalMessage(pm *protocol.Message) (interface{}, error) {
	if pm == nil {
		return nil, nil
	}
	return cn.localSystem.UnmarshalMessage(pm)
}

type clusterNet struct {
	localSystem *system
	// topology 本节点在全互联拓扑中的角色（listen / 需要主动连接的节点下标）。
	// 判定逻辑见 topology.go，与 localSystemIndex 的推导分离，便于单独验证。
	topology             clusterTopology
	clusterConfig        *ClusterConfig
	systemInfos          map[vactor.SystemId]*systemInfo
	systemCount          int
	connectedSystemCount int32
	server               *clusterServer
	clients              map[vactor.SystemId]*clusterClient
}

type systemInfo struct {
	config *SystemConfig
	// weDial 表示"本节点是该连接的发起方"：true 时由本节点的 client 主动连对方
	// （收发走 info.cli），false 时由对方连入本节点的 server（收发走 info.session）。
	// 即 SystemConfigs 列表中**排在自身之前**的节点为 true（见 topology.weDial）。
	//
	// 该字段原名 passive，语义恰好相反（"passive" 的那一方实际上是我们主动去连的），
	// 是这段代码最大的理解成本来源，故改名。
	weDial  bool
	lock    sync.RWMutex
	session netSession.NetSession
	cli     netClient.NetClient
}

// DefaultConnectTimeout 是等待全员互连的兜底时限（仅在用户没配
// ClusterConfig.ConnectTimeout 时生效）。
//
// 原默认是"无限等待"：Host 写错一个字符、或某个节点根本没起来时，Start() 会永远
// 阻塞在下面的等待里，既不报错也不返回——本轮写测试时真实踩到（测试包直接挂到
// go test 超时）。要显式保留无限等待的行为，把 ConnectTimeout 设成负值。
const DefaultConnectTimeout = 30 * time.Second

// connectWaitTimeout 返回本次启动等待互连的时限；<0 表示无限等待。
func (cn *clusterNet) connectWaitTimeout() time.Duration {
	if cn.clusterConfig.ConnectTimeout > 0 {
		return cn.clusterConfig.ConnectTimeout
	}
	if cn.clusterConfig.ConnectTimeout < 0 {
		return -1
	}
	if len(cn.clusterConfig.SystemConfigs) <= 1 {
		return -1 // 单节点无需互连，直接过
	}
	return DefaultConnectTimeout
}

// connectWaitInterval 是互连进度的轮询间隔。原来固定 3s：既让"早就连上"的情况
// 平均多等 1.5s，又要在等待期间反复打同一行日志。
const connectWaitInterval = 100 * time.Millisecond

// start 启动集群网络并等待全员互连。
func (cn *clusterNet) start() error {
	cn.localSystem.LogDebug("start cluster")
	atomic.AddInt32(&cn.connectedSystemCount, 1)
	if cn.topology.listen {
		cn.localSystem.LogInfo("start server")
		cn.server = NewServer(cn)
		if err := cn.server.Start(); err != nil {
			return err
		}
	}

	if len(cn.topology.dialIndexes) > 0 {
		cn.clients = make(map[vactor.SystemId]*clusterClient, len(cn.topology.dialIndexes))
		for _, index := range cn.topology.dialIndexes {
			config := cn.clusterConfig.SystemConfigs[index]
			client := NewClusterClient(cn, config.SystemId)
			cn.clients[config.SystemId] = client
			cn.localSystem.LogInfo("start client to %v", config.SystemId)
			client.Start()
		}
	}

	timeout := cn.connectWaitTimeout()
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	ticker := time.NewTicker(connectWaitInterval)
	defer ticker.Stop()
	lastReported := int32(-1)
	for {
		connectedSystemCount := atomic.LoadInt32(&cn.connectedSystemCount)
		if connectedSystemCount >= int32(cn.systemCount) {
			break
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return fmt.Errorf("wait cluster connected timeout after %v (%v/%v)", timeout, connectedSystemCount, cn.systemCount)
		}
		if connectedSystemCount != lastReported {
			// 只在进度变化时打一行，避免无限等待把日志刷满
			cn.localSystem.LogInfo("wait connected %v/%v", connectedSystemCount, cn.systemCount)
			lastReported = connectedSystemCount
		}
		<-ticker.C
	}

	cn.localSystem.LogInfo("start cluster success")
	return nil
}

// stop 停止集群网络：终止所有重连循环、关闭监听与已建立的会话（幂等）。
//
//	clients 的 Stop 会等各自重连循环退出（有界），保证返回后不再有 goroutine
//	持有打开的 socket。
func (cn *clusterNet) stop() {
	for _, client := range cn.clients {
		client.Stop()
	}
	if cn.server != nil {
		cn.server.Stop()
	}
}

// messageSender 抽象"主动连接侧(client)"与"被动接入侧(session)"共同的发送入口，
// 让 doSend 能在锁外调用它。
type messageSender interface {
	SendMessage(msgId uint32, data []byte) error
}

// sender 在锁内只取快照，真正的发送放在锁外由调用方执行。
//
// 为什么不能持锁发送：SendMessage 最终落到 TCP writer。对端停止读取时它会耗时更久
// （有界队列下会立刻返回队满，但停机路径上的 Close 仍要等一把写锁），持 RLock 期间
// 该系统的重连/绑定清理（要写锁）会一起被拖住——一条慢链路就能阻塞整个方向的自愈。
// 返回 nil 表示该方向当前没有可用连接。
func (si *systemInfo) sender() messageSender {
	si.lock.RLock()
	defer si.lock.RUnlock()
	if si.weDial {
		if si.cli == nil {
			return nil
		}
		return si.cli
	}
	if si.session == nil {
		return nil
	}
	return si.session
}

func (cn *clusterNet) doSend(systemId vactor.SystemId, msgId uint32, data []byte) vactor.VAError {
	info, ok := cn.systemInfos[systemId]
	if !ok || info == nil {
		// 目标系统未在集群配置中声明。必须返回错误而不是继续解引用：调用方可能
		// 在系统外（main/业务 goroutine），那里没有 recover 兜底，panic 会直接杀进程。
		cn.localSystem.LogError("system %v is not declared in cluster config", systemId)
		return vactor.NewVAError(ErrorCodeUnknownSystem)
	}
	sender := info.sender()
	if sender == nil {
		// 与旧行为一致：未连接按发送失败上报（此前是"发送后补一句 disconnect"，
		// 结果同一条链路两种日志，且发送与判空分居两处容易再写错）
		cn.localSystem.LogError("system %v disconnect", systemId)
		return vactor.NewVAError(ErrorCodeMessageSendFail)
	}
	if err := sender.SendMessage(msgId, data); err != nil {
		// 错误文本自带原因（"send queue is full" / "connection closed"），
		// 队满说明对端跟不上：这里是唯一的可见点，不能只留一条无上下文的日志
		cn.localSystem.LogError("system %v send message error: %v", systemId, err)
		return vactor.NewVAError(ErrorCodeMessageSendFail)
	}
	return nil
}

func (cn *clusterNet) Send(systemId vactor.SystemId, envelope vactor.Envelope) vactor.VAError {
	var msgId uint32
	var pkg proto.Message

	switch e := envelope.(type) {
	case *vactor.EnvelopeSend:
		msg, err := cn.marshalMessage(e.Message)
		if err != nil {
			return err
		}
		msgId = uint32(protocol.PkgType_PkgTypeEnvelopeSend)
		pkg = &protocol.PkgEnvelopeSend{
			FromActorRef: ActorRefToProto(e.FromActorRef),
			ToActorRef:   ActorRefToProto(e.ToActorRef),
			Message:      msg,
		}
	case *vactor.EnvelopeBatchSend:
		messages := make([]*protocol.Message, 0, len(e.Messages))
		for _, message := range e.Messages {
			msg, err := cn.marshalMessage(message)
			if err != nil {
				cn.localSystem.LogError("MarshalMessage error: %v\n", err)
				// 整批拒绝：部分投递无法回滚，调用方也无从得知哪些到达了
				return err
			}
			messages = append(messages, msg)
		}
		if len(messages) == 0 {
			cn.localSystem.LogError("batch send aborted: no message to send")
			return vactor.NewVAError(ErrorCodeMessageLenError)
		}
		stringActorRefs := make([]*protocol.ActorRef, len(e.ToActorRefs))
		for i, key := range e.ToActorRefs {
			stringActorRefs[i] = ActorRefToProto(key)
		}
		msgId = uint32(protocol.PkgType_PkgTypeEnvelopeBatchSend)
		pkg = &protocol.PkgEnvelopeBatchSend{
			FromActorRef: ActorRefToProto(e.FromActorRef),
			ToActorRefs:  stringActorRefs,
			Messages:     messages,
		}
	case *vactor.EnvelopeRequestAsync:
		msg, err := cn.marshalMessage(e.Message)
		if err != nil {
			return err
		}
		msgId = uint32(protocol.PkgType_PkgTypeEnvelopeRequestAsync)
		pkg = &protocol.PkgEnvelopeRequestAsync{
			FromActorRef:    ActorRefToProto(e.FromActorRef),
			ToActorRef:      ActorRefToProto(e.ToActorRef),
			Message:         msg,
			CallbackId:      uint64(e.CallbackId),
			CallbackAddress: e.CallbackAddress,
		}
	case *vactor.EnvelopeResponseAsync:
		msg, err := cn.marshalMessage(e.Message)
		if err != nil {
			return err
		}
		msgId = uint32(protocol.PkgType_PkgTypeEnvelopeResponseAsync)
		rsp := &protocol.Response{
			Message: msg,
		}
		if e.Error == nil {
			rsp.ErrorCode = protocol.ErrorCode_ErrorCodeSuccess
		} else {
			rsp.ErrorCode = protocol.ErrorCode(e.Error.Code())
		}
		pkg = &protocol.PkgEnvelopeResponseAsync{
			FromActorRef:    ActorRefToProto(e.FromActorRef),
			ToActorRef:      ActorRefToProto(e.ToActorRef),
			Response:        rsp,
			CallbackId:      uint64(e.CallbackId),
			CallbackAddress: e.CallbackAddress,
		}
	case *vactor.EnvelopeRequest:
		msg, err := cn.marshalMessage(e.Message)
		if err != nil {
			return err
		}
		msgId = uint32(protocol.PkgType_PkgTypeEnvelopeRequest)
		pkg = &protocol.PkgEnvelopeRequest{
			FromActorRef:    ActorRefToProto(e.FromActorRef),
			ToActorRef:      ActorRefToProto(e.ToActorRef),
			Message:         msg,
			RequestId:       uint64(e.RequestId),
			CallbackAddress: e.CallbackAddress,
		}
	case *vactor.EnvelopeResponse:
		msg, err := cn.marshalMessage(e.Message)
		if err != nil {
			return err
		}
		msgId = uint32(protocol.PkgType_PkgTypeEnvelopeResponse)
		rsp := &protocol.Response{
			Message: msg,
		}
		if e.Error == nil {
			rsp.ErrorCode = protocol.ErrorCode_ErrorCodeSuccess
		} else {
			rsp.ErrorCode = protocol.ErrorCode(e.Error.Code())
		}
		pkg = &protocol.PkgEnvelopeResponse{
			FromActorRef:    ActorRefToProto(e.FromActorRef),
			ToActorRef:      ActorRefToProto(e.ToActorRef),
			Response:        rsp,
			RequestId:       uint64(e.RequestId),
			CallbackAddress: e.CallbackAddress,
		}
	case *vactor.EnvelopeWatch:
		msgId = uint32(protocol.PkgType_PkgTypeEnvelopeWatch)
		pkg = &protocol.PkgEnvelopeWatch{
			FromActorRef: ActorRefToProto(e.FromActorRef),
			ToActorRef:   ActorRefToProto(e.ToActorRef),
			WatchType:    uint32(e.WatchType),
			IsWatch:      e.IsWatch,
		}
	case *vactor.EnvelopeNotify:
		// e.Message 为 nil 时不能解引用：正常内部路径总会构造它，但信封是导出类型，
		// 手工构造并交给 LocalRouter 的调用方会走到这里。
		if e.Message == nil {
			cn.localSystem.LogError("envelope notify without payload, dropped")
			return vactor.NewVAError(ErrorCodeMessageLenError)
		}
		// 与 Request/Response 一致走 marshalMessage：nil 消息编码为"未携带"，
		// 接收侧还原为 nil。直接用 MarshalMessage 会让本地能发的 nil/非 proto
		// 通知在跨节点时整批失败，语义不一致。
		msg, err := cn.marshalMessage(e.Message.Message)
		if err != nil {
			return err
		}
		msgId = uint32(protocol.PkgType_PkgTypeEnvelopeNotify)
		toActorRefs := make([]*protocol.ActorRef, len(e.ToActorRefs))
		for i, key := range e.ToActorRefs {
			toActorRefs[i] = ActorRefToProto(key)
		}
		pkg = &protocol.PkgEnvelopeNotify{
			FromActorRef: ActorRefToProto(e.FromActorRef),
			ToActorRefs:  toActorRefs,
			NotifyType:   uint32(e.NotifyType),
			ActorRef:     ActorRefToProto(e.Message.ActorRef),
			WatchType:    uint32(e.Message.WatchType),
			Message:      msg,
		}
	case *vactor.EnvelopeFireNotify:
		msg, err := cn.marshalMessage(e.Message)
		if err != nil {
			return err
		}
		msgId = uint32(protocol.PkgType_PkgTypeEnvelopeFireNotify)
		pkg = &protocol.PkgEnvelopeFireNotify{
			FromActorRef: ActorRefToProto(e.FromActorRef),
			ToActorRef:   ActorRefToProto(e.ToActorRef),
			NotifyType:   uint32(e.NotifyType),
			WatchType:    uint32(e.WatchType),
			Message:      msg,
		}
	default:
		cn.localSystem.LogError("unknown envelope type")
		return vactor.NewVAError(ErrorCodeUnknownEnvelope)
	}
	data, err := proto.Marshal(pkg)
	if err != nil {
		return vactor.NewVAError(ErrorCodeMessageSerializeFail)
	}
	return cn.doSend(systemId, msgId, data)
}

// dispatchSafely 运行业务消息处理，把其中的 panic 转成错误并留日志。
//
// 需要这层兜底有两个理由：① 网络读 goroutine 里的 panic 会击穿整个进程；② panic
// 若只被 engine 层（socket/conn.go 的 safeOnData）吃掉，只会降级成一个 error，而
// engine 层没有日志设施——堆栈就此丢失，线上再也查不到是谁 panic 的。所以在本层
// （有 logger 的地方）把堆栈打全，再返回 error 让 engine 走"断这一条链路"的既有
// 失败路径收尾。返回 error 而非 nil 是因为 panic 之后该连接的状态已不可信。
func (cn *clusterNet) dispatchSafely(handle func() error, what string, msgId uint32) (err error) {
	defer func() {
		if r := recover(); r != nil {
			cn.localSystem.LogError("%s msgId=%v panic: %v\n%s", what, msgId, r, debug.Stack())
			err = fmt.Errorf("%s panic: %v", what, r)
		}
	}()
	return handle()
}

// logDecodeFailure 记录一个被丢弃的业务包。
//
// 刻意不把错误返回给 engine：engine 把 OnMessage 的 error 当作"这条链路不可信、
// 断开"的信号（见 cluster_server.go / client.go 的调用点）。但"本节点没注册这个
// 消息类型""单个信封解不开"都是**这一条消息**的问题，与链路是否可信无关。
// 按 error 上抛的旧行为等于让一条不兼容消息拆掉整条集群链路：漏注册一个节点或
// 滚动升级期间会形成重连风暴（5s 退避 + 10s 注册窗口），期间该链路上的全部消息
// 都在丢。真正该断链的是帧长不可信——那发生在 engine 的 splitter 层，不经这里。
func (cn *clusterNet) logDecodeFailure(msgId uint32, err error) {
	cn.localSystem.LogError("cluster packet msgId=%v dropped: %v", msgId, err)
}

func (cn *clusterNet) OnMessage(msgId uint32, data []byte) error {
	switch protocol.PkgType(msgId) {
	case protocol.PkgType_PkgTypeEnvelopeSend:
		pkg := &protocol.PkgEnvelopeSend{}
		err := proto.Unmarshal(data, pkg)
		if err != nil {
			cn.logDecodeFailure(msgId, err)
			return nil
		}
		msg, err := cn.unmarshalMessage(pkg.Message)
		if err != nil {
			cn.logDecodeFailure(msgId, err)
			return nil
		}
		_ = cn.localSystem.LocalRouter(&vactor.EnvelopeSend{
			FromActorRef: ActorRefFromProto(pkg.FromActorRef),
			ToActorRef:   cn.localTarget(pkg.ToActorRef),
			Message:      msg,
		})
	case protocol.PkgType_PkgTypeEnvelopeBatchSend:
		pkg := &protocol.PkgEnvelopeBatchSend{}
		err := proto.Unmarshal(data, pkg)
		if err != nil {
			cn.logDecodeFailure(msgId, err)
			return nil
		}
		msgs := make([]interface{}, 0, len(pkg.Messages))
		for _, protoMsg := range pkg.Messages {
			msg, err := cn.unmarshalMessage(protoMsg)
			if err != nil {
				cn.localSystem.LogError("UnmarshalMessage error: %v", err)
				continue
			}
			msgs = append(msgs, msg)
		}
		toActorRefs := cn.localTargets(pkg.ToActorRefs)
		e := &vactor.EnvelopeBatchSend{
			FromActorRef: ActorRefFromProto(pkg.FromActorRef),
			ToActorRefs:  toActorRefs,
			Messages:     msgs,
		}
		_ = cn.localSystem.LocalRouter(e)
	case protocol.PkgType_PkgTypeEnvelopeRequestAsync:
		pkg := &protocol.PkgEnvelopeRequestAsync{}
		err := proto.Unmarshal(data, pkg)
		if err != nil {
			cn.logDecodeFailure(msgId, err)
			return nil
		}
		msg, err := cn.unmarshalMessage(pkg.Message)
		if err != nil {
			cn.logDecodeFailure(msgId, err)
			return nil
		}
		e := &vactor.EnvelopeRequestAsync{
			FromActorRef:    ActorRefFromProto(pkg.FromActorRef),
			ToActorRef:      cn.localTarget(pkg.ToActorRef),
			Message:         msg,
			CallbackId:      vactor.CallbackId(pkg.CallbackId),
			CallbackAddress: pkg.CallbackAddress,
		}
		_ = cn.localSystem.LocalRouter(e)
	case protocol.PkgType_PkgTypeEnvelopeResponseAsync:
		pkg := &protocol.PkgEnvelopeResponseAsync{}
		err := proto.Unmarshal(data, pkg)
		if err != nil {
			cn.logDecodeFailure(msgId, err)
			return nil
		}
		var msg interface{}
		// 对端可能不填 Response（nil）：此处不能再解引用它，按成功码处理
		var rspCode protocol.ErrorCode
		if pkg.Response != nil {
			if msg, err = cn.unmarshalMessage(pkg.Response.Message); err != nil {
				cn.logDecodeFailure(msgId, err)
				return nil
			}
			rspCode = pkg.Response.ErrorCode
		}
		e := &vactor.EnvelopeResponseAsync{
			FromActorRef: ActorRefFromProto(pkg.FromActorRef),
			ToActorRef:   cn.localTarget(pkg.ToActorRef),
			Response: &vactor.Response{
				Error:   errorCodeToVAError(rspCode),
				Message: msg,
			},
			CallbackId:      vactor.CallbackId(pkg.CallbackId),
			CallbackAddress: pkg.CallbackAddress,
		}
		_ = cn.localSystem.LocalRouter(e)
	case protocol.PkgType_PkgTypeEnvelopeRequest:
		pkg := &protocol.PkgEnvelopeRequest{}
		err := proto.Unmarshal(data, pkg)
		if err != nil {
			cn.logDecodeFailure(msgId, err)
			return nil
		}
		msg, err := cn.unmarshalMessage(pkg.Message)
		if err != nil {
			cn.logDecodeFailure(msgId, err)
			return nil
		}
		e := &vactor.EnvelopeRequest{
			FromActorRef:    ActorRefFromProto(pkg.FromActorRef),
			ToActorRef:      cn.localTarget(pkg.ToActorRef),
			Message:         msg,
			RequestId:       vactor.CallbackId(pkg.RequestId),
			CallbackAddress: pkg.CallbackAddress,
		}
		_ = cn.localSystem.LocalRouter(e)
	case protocol.PkgType_PkgTypeEnvelopeResponse:
		pkg := &protocol.PkgEnvelopeResponse{}
		err := proto.Unmarshal(data, pkg)
		if err != nil {
			cn.logDecodeFailure(msgId, err)
			return nil
		}
		var msg interface{}
		// 同上：Response 字段可能缺失，缺失时按成功码还原
		var rspCode protocol.ErrorCode
		if pkg.Response != nil {
			if msg, err = cn.unmarshalMessage(pkg.Response.Message); err != nil {
				cn.logDecodeFailure(msgId, err)
				return nil
			}
			rspCode = pkg.Response.ErrorCode
		}
		e := &vactor.EnvelopeResponse{
			FromActorRef: ActorRefFromProto(pkg.FromActorRef),
			ToActorRef:   cn.localTarget(pkg.ToActorRef),
			RequestId:    vactor.CallbackId(pkg.RequestId),
			Response: &vactor.Response{
				Error:   errorCodeToVAError(rspCode),
				Message: msg,
			},
			CallbackAddress: pkg.CallbackAddress,
		}
		_ = cn.localSystem.LocalRouter(e)
	case protocol.PkgType_PkgTypeEnvelopeWatch:
		pkg := &protocol.PkgEnvelopeWatch{}
		err := proto.Unmarshal(data, pkg)
		if err != nil {
			cn.logDecodeFailure(msgId, err)
			return nil
		}
		e := &vactor.EnvelopeWatch{
			FromActorRef: ActorRefFromProto(pkg.FromActorRef),
			ToActorRef:   cn.localTarget(pkg.ToActorRef),
			WatchType:    vactor.WatchType(pkg.WatchType),
			IsWatch:      pkg.IsWatch,
		}
		_ = cn.localSystem.LocalRouter(e)
	case protocol.PkgType_PkgTypeEnvelopeNotify:
		pkg := &protocol.PkgEnvelopeNotify{}
		err := proto.Unmarshal(data, pkg)
		if err != nil {
			cn.logDecodeFailure(msgId, err)
			return nil
		}
		msg, err := cn.unmarshalMessage(pkg.Message)
		if err != nil {
			cn.logDecodeFailure(msgId, err)
			return nil
		}
		toActorRefs := cn.localTargets(pkg.ToActorRefs)
		e := &vactor.EnvelopeNotify{
			FromActorRef: ActorRefFromProto(pkg.FromActorRef),
			ToActorRefs:  toActorRefs,
			NotifyType:   vactor.NotifyType(pkg.NotifyType),
			Message: &vactor.MsgOnWatchMsg{
				ActorRef:  ActorRefFromProto(pkg.ActorRef),
				WatchType: vactor.WatchType(pkg.WatchType),
				Message:   msg,
			},
		}
		_ = cn.localSystem.LocalRouter(e)
	case protocol.PkgType_PkgTypeEnvelopeFireNotify:
		pkg := &protocol.PkgEnvelopeFireNotify{}
		err := proto.Unmarshal(data, pkg)
		if err != nil {
			cn.logDecodeFailure(msgId, err)
			return nil
		}
		msg, err := cn.unmarshalMessage(pkg.Message)
		if err != nil {
			cn.logDecodeFailure(msgId, err)
			return nil
		}
		e := &vactor.EnvelopeFireNotify{
			FromActorRef: ActorRefFromProto(pkg.FromActorRef),
			ToActorRef:   cn.localTarget(pkg.ToActorRef),
			NotifyType:   vactor.NotifyType(pkg.NotifyType),
			WatchType:    vactor.WatchType(pkg.WatchType),
			Message:      msg,
		}
		_ = cn.localSystem.LocalRouter(e)
	default:
		cn.localSystem.LogError("unknown pkg type %v", msgId)
	}
	return nil
}
