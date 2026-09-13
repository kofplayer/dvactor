package dvactor

import (
	"sync"
	"testing"
	"time"

	netConnect "github.com/kofplayer/dvactor/engine/net/connect"
	netSession "github.com/kofplayer/dvactor/engine/net/session"
	"github.com/kofplayer/dvactor/protocol"
	"github.com/kofplayer/vactor"
	"google.golang.org/protobuf/proto"
)

// 回归测试：成功错误码必须映射为 nil（否则跨节点成功响应被误判为失败）
func TestErrorCodeToVAError(t *testing.T) {
	if err := errorCodeToVAError(protocol.ErrorCode_ErrorCodeSuccess); err != nil {
		t.Fatalf("success code should map to nil, got %v", err)
	}
	err := errorCodeToVAError(protocol.ErrorCode_ErrorCodeTimeout)
	if err == nil {
		t.Fatal("timeout code should map to non-nil VAError")
	}
	if err.Code() != vactor.ErrorCodeTimeout {
		t.Fatalf("expected ErrorCodeTimeout, got %v", err.Code())
	}
}

// 回归测试：actorType 未在任何节点声明时不得除零 panic，应回落本机
func TestRouterFallbackWhenTypeUndeclared(t *testing.T) {
	cfg := &ClusterConfig{
		LocalSystemId: 1,
		SystemConfigs: []*SystemConfig{
			{SystemId: 1, Host: "localhost", Port: 8001, ActorTypes: []vactor.ActorType{}},
		},
	}
	router := NewRouter(vactor.NewSystem(), cfg, nil)
	ref := router.CreateActorRefEx(0, vactor.ActorType(999), "1")
	if ref.GetSystemId() != 1 {
		t.Fatalf("should fallback to local system 1, got %v", ref.GetSystemId())
	}
}

// 线协议编解码：单帧、粘包、半包、msgId 越界
func TestPackAndSplit(t *testing.T) {
	pkt1, err := netConnect.PackMessage(1, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	pkt2, err := netConnect.PackMessage(11, []byte("world!!"))
	if err != nil {
		t.Fatal(err)
	}

	// msgId 越界必须报错而不是静默截断
	if _, err = netConnect.PackMessage(256, nil); err == nil {
		t.Fatal("msgId 256 should return error")
	}

	var sp netConnect.PacketSplitter
	stream := append(pkt1, pkt2...)
	// 模拟半包：先给 3 字节，再给剩余
	sp.Append(stream[:3])
	if _, _, ok, _ := sp.Next(); ok {
		t.Fatal("should not emit frame on partial header")
	}
	// 模拟粘包：剩余字节一次给齐（含两帧）
	sp.Append(stream[3:])

	id, payload, ok, err := sp.Next()
	if !ok || id != 1 || string(payload) != "hello" {
		t.Fatalf("frame1 mismatch: id=%v payload=%q ok=%v", id, payload, ok)
	}
	id, payload, ok, err = sp.Next()
	if !ok || id != 11 || string(payload) != "world!!" {
		t.Fatalf("frame2 mismatch: id=%v payload=%q ok=%v", id, payload, ok)
	}
	if _, _, ok, _ := sp.Next(); ok {
		t.Fatal("no more frames expected")
	}
}

// 回归测试：重复注册消息类型不得 panic，且以最后一次为准
func TestRegisterMessageTypeDuplicate(t *testing.T) {
	s := NewSystem(&ClusterConfig{
		LocalSystemId: 1,
		SystemConfigs: []*SystemConfig{{SystemId: 1, ActorTypes: []vactor.ActorType{}}},
	}).(*system)
	s.RegisterMessageType(1, func() proto.Message { return &protocol.Message{} })
	s.RegisterMessageType(1, func() proto.Message { return &protocol.ActorRef{} })
	if got := s.msgCreators[1](); got == nil {
		t.Fatal("creator should exist")
	} else if _, ok := got.(*protocol.ActorRef); !ok {
		t.Fatalf("last registration should win, got %T", got)
	}
}

// 回归测试：对端未填 Response 字段时不得 panic。
// 修复前：上面判了 pkg.Response != nil，下面却无条件解引用 pkg.Response.ErrorCode。
func TestOnMessageToleratesMissingResponse(t *testing.T) {
	s := NewSystem(&ClusterConfig{
		LocalSystemId: 1,
		SystemConfigs: []*SystemConfig{{SystemId: 1, ActorTypes: nil}},
	}).(*system)
	s.Start()
	defer s.Stop()

	build := func(msg proto.Message) []byte {
		data, err := proto.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return data
	}
	ref := func(actorType uint32, id string) *protocol.ActorRef {
		return &protocol.ActorRef{SystemId: 1, GroupSlot: 1, ActorType: actorType, ActorId: id}
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic on missing Response field: %v", r)
		}
	}()

	// 异步响应包：Response 字段缺失
	_ = s.clusterNet.OnMessage(uint32(protocol.PkgType_PkgTypeEnvelopeResponseAsync),
		build(&protocol.PkgEnvelopeResponseAsync{
			FromActorRef:    ref(100, "a"),
			ToActorRef:      ref(101, "b"),
			Response:        nil,
			CallbackId:      1,
			CallbackAddress: 1,
		}))
	// 同步响应包：Response 字段缺失
	_ = s.clusterNet.OnMessage(uint32(protocol.PkgType_PkgTypeEnvelopeResponse),
		build(&protocol.PkgEnvelopeResponse{
			FromActorRef: ref(100, "a"),
			ToActorRef:   ref(101, "b"),
			RequestId:    1,
			Response:     nil,
		}))
}

// 回归测试：每轮重连开始时必须清空上一轮残留的注册响应，
// 否则迟到的旧响应会被本轮误当作"注册成功"（集群假连通）。
func TestClusterClientDrainsStaleRegisterResponse(t *testing.T) {
	c := NewClusterClient(nil, 1)
	c.registerResponseChan <- true // 模拟上一轮残留
	c.drainRegisterResponse()
	select {
	case v := <-c.registerResponseChan:
		t.Fatalf("stale register response survived drain: %v", v)
	default:
	}
}

// 回归测试：断线信号满时不得阻塞网络回调 goroutine。
// 修复前是阻塞写，连续断线会永久卡住接收 goroutine，连接彻底失效。
func TestClusterClientDisconnectSignalNonBlocking(t *testing.T) {
	c := NewClusterClient(nil, 1)
	c.disconnectChan <- true // 缓冲（1）已占满
	done := make(chan struct{})
	go func() {
		c.OnDisconnect()
		c.OnDisconnect()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OnDisconnect blocked on a full channel")
	}
}

// 空实现的回调必须可以安全调用（覆盖 client/server 的 OnConnect、session 的
// GetConn/Close 等此前零覆盖的边角路径）。
func TestNoopCallbacksAndSessionAccessors(t *testing.T) {
	cli := NewClusterClient(nil, 1)
	cli.OnConnect() // 空实现，不得 panic

	svr := &clusterServer{}
	svr.OnConnect(nil) // 空实现，不得 panic

	sm := netSession.NewSessionMgr()
	s := sm.NewSession()
	if s.GetConn() != nil {
		t.Fatal("a fresh session should have no conn")
	}
	if s.GetID() == 0 {
		t.Fatal("session id should be assigned")
	}
	if got := sm.GetSession(s.GetID()); got != s {
		t.Fatal("GetSession should return the created session")
	}
	if got := sm.GetSession(9999); got != nil {
		t.Fatal("GetSession with unknown id should return nil")
	}
	visited := 0
	sm.TravelSession(func(netSession.NetSession) bool {
		visited++
		return true
	})
	if visited != 1 {
		t.Fatalf("TravelSession visited %d, want 1", visited)
	}
	if err := s.Close(); err != nil { // conn == nil：直接返回 nil
		t.Fatalf("Close without conn: %v", err)
	}
	// 绑定对象存取
	s.SetBindObject("bound")
	if s.GetBindObject() != "bound" {
		t.Fatal("bind object roundtrip failed")
	}
	sm.RemoveSession(s.GetID())
	if got := sm.GetSession(s.GetID()); got != nil {
		t.Fatal("session should be removed")
	}
}

// GetWatcheeActorRef 对非法 ActorId（缺少分隔符或类型段非数字）必须返回 nil。
func TestGetWatcheeActorRefRejectsMalformedId(t *testing.T) {
	if ref := GetWatcheeActorRef(nil, "no-dash-missing"); ref != nil {
		t.Fatalf("id without '-' should be rejected, got %v", ref)
	}
	if ref := GetWatcheeActorRef(nil, "abc-1"); ref != nil {
		t.Fatalf("non-numeric type segment should be rejected, got %v", ref)
	}
	if ref := GetWatcheeActorRef(nil, "99999999999-1"); ref != nil {
		t.Fatalf("out-of-range type segment should be rejected, got %v", ref)
	}
}

// refreshWatches 在还没有 watchee 引用（代理尚未收到 MsgOnStart）时必须安全返回。
func TestWatchProxyRefreshWithoutWatchee(t *testing.T) {
	wp := NewWatchProxy()
	wp.refreshWatches(nil) // watcheeActorRef == nil：直接返回
	if wp.isWatch(vactor.WatchType(1)) {
		t.Fatal("no subscription should be reported")
	}
}

// unknownEnvelope 用于覆盖 Send 的"未知信封类型"分支。
type unknownEnvelope struct{}

func (unknownEnvelope) GetToActorRef() vactor.ActorRef { return nil }

func newCodecSystem(t *testing.T) *system {
	t.Helper()
	s := NewSystem(&ClusterConfig{
		LocalSystemId: 1,
		SystemConfigs: []*SystemConfig{
			{SystemId: 1, Port: 19981, ActorTypes: []vactor.ActorType{ActorTypeStart + 30}},
			{SystemId: 2, Host: "127.0.0.1", Port: 19982, ActorTypes: []vactor.ActorType{ActorTypeStart + 30}},
		},
	}, func(sc *vactor.SystemConfig) {
		sc.LogFunc = func(vactor.LogLevel, string, ...interface{}) {}
	}).(*system)
	// 线协议要求消息类型已注册（这里直接复用 protocol.Message 作为载荷）
	s.RegisterMessageType(1, func() proto.Message { return &protocol.Message{} })
	return s
}

// Send 必须为每一种信封都走到对应编码分支；远端未连接时返回错误但不得 panic。
func TestClusterNetSendEncodesEveryEnvelopeKind(t *testing.T) {
	s := newCodecSystem(t)
	cn := s.clusterNet
	const dst vactor.SystemId = 2
	self := func(id string) vactor.ActorRef {
		return &vactor.ActorRefImpl{SystemId: 1, GroupSlot: 1, ActorType: ActorTypeStart + 30, ActorId: vactor.ActorId(id)}
	}
	remote := func(id string) vactor.ActorRef {
		return &vactor.ActorRefImpl{SystemId: 2, GroupSlot: 1, ActorType: ActorTypeStart + 30, ActorId: vactor.ActorId(id)}
	}
	payload := func() interface{} { return &protocol.Message{Type: 1, Data: []byte("x")} }

	vcErr := vactor.NewVAError(vactor.ErrorCodeTimeout)
	cases := []struct {
		name string
		env  vactor.Envelope
	}{
		{"send", &vactor.EnvelopeSend{FromActorRef: self("a"), ToActorRef: remote("b"), Message: payload()}},
		{"batchsend", &vactor.EnvelopeBatchSend{FromActorRef: self("a"), ToActorRefs: []vactor.ActorRef{remote("b"), remote("c")}, Messages: []interface{}{payload(), payload()}}},
		{"requestasync", &vactor.EnvelopeRequestAsync{FromActorRef: self("a"), ToActorRef: remote("b"), Message: payload(), CallbackId: 1, CallbackAddress: 1}},
		{"responseasync-ok", &vactor.EnvelopeResponseAsync{FromActorRef: self("a"), ToActorRef: remote("b"), Response: &vactor.Response{Message: payload()}, CallbackId: 1, CallbackAddress: 1}},
		{"responseasync-err", &vactor.EnvelopeResponseAsync{FromActorRef: self("a"), ToActorRef: remote("b"), Response: &vactor.Response{Error: vcErr}, CallbackId: 1, CallbackAddress: 1}},
		{"request", &vactor.EnvelopeRequest{FromActorRef: self("a"), ToActorRef: remote("b"), Message: payload(), RequestId: 7}},
		{"response-ok", &vactor.EnvelopeResponse{FromActorRef: self("a"), ToActorRef: remote("b"), Response: &vactor.Response{Message: payload()}, RequestId: 7}},
		{"response-err", &vactor.EnvelopeResponse{FromActorRef: self("a"), ToActorRef: remote("b"), Response: &vactor.Response{Error: vcErr}, RequestId: 7}},
		{"watch", &vactor.EnvelopeWatch{FromActorRef: self("a"), ToActorRef: remote("b"), WatchType: 1, IsWatch: true}},
		{"notify", &vactor.EnvelopeNotify{FromActorRef: self("a"), ToActorRefs: []vactor.ActorRef{remote("b")}, NotifyType: vactor.NotifyTypeWatch, Message: &vactor.MsgOnWatchMsg{ActorRef: self("a"), WatchType: 1, Message: payload()}}},
		{"firenotify", &vactor.EnvelopeFireNotify{FromActorRef: self("a"), ToActorRef: remote("b"), NotifyType: vactor.NotifyTypeEvent, WatchType: 1, Message: payload()}},
		{"unknown", unknownEnvelope{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Send(%s) panicked: %v", c.name, r)
				}
			}()
			// 远端未连接，返回错误是预期行为；这里只要求编码分支完整走通
			_ = cn.Send(dst, c.env)
		})
	}
	// 未注册的消息类型必须返回明确错误
	err := cn.Send(dst, &vactor.EnvelopeSend{FromActorRef: self("a"), ToActorRef: remote("b"), Message: "not-a-proto-message"})
	if err == nil {
		t.Fatal("unregistered message type should return an error")
	}
}

// OnMessage 必须能从线协议还原每一种包；字段缺失（Response 为 nil）时也不得 panic。
func TestClusterNetOnMessageHandlesEveryPkgType(t *testing.T) {
	s := newCodecSystem(t)
	cn := s.clusterNet
	ref := func(tp uint32, id string) *protocol.ActorRef {
		return &protocol.ActorRef{SystemId: 1, GroupSlot: 1, ActorType: tp, ActorId: id}
	}
	const at = uint32(ActorTypeStart + 30)
	// 载荷必须是合法序列化的 protobuf（接收侧会用注册的 creator 反序列化）
	inner, err := proto.Marshal(&protocol.Message{Type: 1, Data: []byte("payload")})
	if err != nil {
		t.Fatalf("build payload: %v", err)
	}
	pmsg := &protocol.Message{Type: 1, Data: inner}

	deliver := func(name string, msgId uint32, m proto.Message) {
		t.Helper()
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("OnMessage(%s) panicked: %v", name, r)
			}
		}()
		data, err := proto.Marshal(m)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		if err := cn.OnMessage(msgId, data); err != nil {
			t.Fatalf("OnMessage(%s): %v", name, err)
		}
	}

	deliver("send", uint32(protocol.PkgType_PkgTypeEnvelopeSend),
		&protocol.PkgEnvelopeSend{FromActorRef: ref(at, "a"), ToActorRef: ref(at, "b"), Message: pmsg})
	deliver("batchsend", uint32(protocol.PkgType_PkgTypeEnvelopeBatchSend),
		&protocol.PkgEnvelopeBatchSend{ToActorRefs: []*protocol.ActorRef{ref(at, "b")}, Messages: []*protocol.Message{pmsg}})
	deliver("requestasync", uint32(protocol.PkgType_PkgTypeEnvelopeRequestAsync),
		&protocol.PkgEnvelopeRequestAsync{ToActorRef: ref(at, "b"), Message: pmsg, CallbackId: 1, CallbackAddress: 1})
	deliver("responseasync-ok", uint32(protocol.PkgType_PkgTypeEnvelopeResponseAsync),
		&protocol.PkgEnvelopeResponseAsync{ToActorRef: ref(at, "b"), Response: &protocol.Response{ErrorCode: protocol.ErrorCode_ErrorCodeSuccess, Message: pmsg}})
	deliver("responseasync-missing", uint32(protocol.PkgType_PkgTypeEnvelopeResponseAsync),
		&protocol.PkgEnvelopeResponseAsync{ToActorRef: ref(at, "b"), Response: nil})
	deliver("request", uint32(protocol.PkgType_PkgTypeEnvelopeRequest),
		&protocol.PkgEnvelopeRequest{ToActorRef: ref(at, "b"), Message: pmsg, RequestId: 3})
	deliver("response-ok", uint32(protocol.PkgType_PkgTypeEnvelopeResponse),
		&protocol.PkgEnvelopeResponse{ToActorRef: ref(at, "b"), Response: &protocol.Response{ErrorCode: protocol.ErrorCode_ErrorCodeSuccess, Message: pmsg}, RequestId: 3})
	deliver("response-missing", uint32(protocol.PkgType_PkgTypeEnvelopeResponse),
		&protocol.PkgEnvelopeResponse{ToActorRef: ref(at, "b"), Response: nil, RequestId: 3})
	deliver("watch", uint32(protocol.PkgType_PkgTypeEnvelopeWatch),
		&protocol.PkgEnvelopeWatch{ToActorRef: ref(at, "b"), WatchType: 1, IsWatch: true})
	deliver("notify", uint32(protocol.PkgType_PkgTypeEnvelopeNotify),
		&protocol.PkgEnvelopeNotify{ToActorRefs: []*protocol.ActorRef{ref(at, "b")}, NotifyType: 1, ActorRef: ref(at, "a"), WatchType: 1, Message: pmsg})
	deliver("firenotify", uint32(protocol.PkgType_PkgTypeEnvelopeFireNotify),
		&protocol.PkgEnvelopeFireNotify{ToActorRef: ref(at, "b"), NotifyType: 1, WatchType: 1, Message: pmsg})
	// 未知包类型：只记日志，不报错、不 panic
	if err := cn.OnMessage(9999, nil); err != nil {
		t.Fatalf("unknown pkgType should be tolerated, got %v", err)
	}
}

// fakeNetSession 记录 server 侧发出的响应，用于验证注册失败的反馈路径。
type fakeNetSession struct {
	mu     sync.Mutex
	sent   []fakeSentMsg
	closed bool
	bind   interface{}
}

type fakeSentMsg struct {
	msgId uint32
	data  []byte
}

func (f *fakeNetSession) GetID() netSession.SessionID                   { return 1 }
func (f *fakeNetSession) GetConn() netConnect.Conn                      { return nil }
func (f *fakeNetSession) SetConn(netConnect.Conn)                       {}
func (f *fakeNetSession) SetSendMessageFunc(netSession.SendMessageFunc) {}
func (f *fakeNetSession) GetBindObject() interface{}                    { return f.bind }
func (f *fakeNetSession) SetBindObject(o interface{})                   { f.bind = o }
func (f *fakeNetSession) Close() error                                  { f.closed = true; return nil }

func (f *fakeNetSession) SendMessage(msgId uint32, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, fakeSentMsg{msgId, append([]byte(nil), data...)})
	return nil
}

// 鉴权失败的注册必须回带错误码的响应再断开，让 client 立即重试，
// 而不是白等 registerResponseTimeout（10s）才察觉。
func TestAuthRejectionSendsErrorResponse(t *testing.T) {
	s := NewSystem(&ClusterConfig{
		LocalSystemId: 1,
		SystemConfigs: []*SystemConfig{
			{SystemId: 1, Port: 19991, ActorTypes: nil},
			{SystemId: 2, Host: "127.0.0.1", Port: 19992, ActorTypes: nil},
		},
		AuthToken: "right-token",
	}, func(sc *vactor.SystemConfig) {
		sc.LogFunc = func(vactor.LogLevel, string, ...interface{}) {}
	}).(*system)

	svr := NewServer(s.clusterNet)
	fs := &fakeNetSession{}
	data, err := proto.Marshal(&protocol.PkgRegisterSystemReq{SystemId: 2, AuthToken: "wrong-token"})
	if err != nil {
		t.Fatalf("marshal req: %v", err)
	}

	if err := svr.OnMessage(fs, uint32(protocol.PkgType_PkgTypeRegisterSystemReq), data); err == nil {
		t.Fatal("auth mismatch must return an error so the session gets closed")
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.sent) != 1 {
		t.Fatalf("expected exactly 1 response, got %d", len(fs.sent))
	}
	if fs.sent[0].msgId != uint32(protocol.PkgType_PkgTypeRegisterSystemRsp) {
		t.Fatalf("unexpected msgId %d", fs.sent[0].msgId)
	}
	rsp := &protocol.PkgRegisterSystemRsp{}
	if err := proto.Unmarshal(fs.sent[0].data, rsp); err != nil {
		t.Fatalf("unmarshal rsp: %v", err)
	}
	if rsp.ErrorCode != protocol.ErrorCode_ErrorCodeAuthFailed {
		t.Fatalf("expected ErrorCodeAuthFailed, got %v", rsp.ErrorCode)
	}
}

// 未配置 AuthToken 时不做鉴权：注册请求正常进入绑定流程。
func TestRegisterWithoutAuthTokenAllowed(t *testing.T) {
	s := NewSystem(&ClusterConfig{
		LocalSystemId: 1,
		SystemConfigs: []*SystemConfig{
			{SystemId: 1, Port: 19993, ActorTypes: nil},
			{SystemId: 2, Host: "127.0.0.1", Port: 19994, ActorTypes: nil},
		},
	}, func(sc *vactor.SystemConfig) {
		sc.LogFunc = func(vactor.LogLevel, string, ...interface{}) {}
	}).(*system)

	svr := NewServer(s.clusterNet)
	fs := &fakeNetSession{}
	data, err := proto.Marshal(&protocol.PkgRegisterSystemReq{SystemId: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := svr.OnMessage(fs, uint32(protocol.PkgType_PkgTypeRegisterSystemReq), data); err != nil {
		t.Fatalf("register without token should succeed, got %v", err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.sent) != 1 {
		t.Fatalf("expected success response, got %d", len(fs.sent))
	}
	rsp := &protocol.PkgRegisterSystemRsp{}
	if err := proto.Unmarshal(fs.sent[0].data, rsp); err != nil {
		t.Fatal(err)
	}
	if rsp.ErrorCode != protocol.ErrorCode_ErrorCodeSuccess {
		t.Fatalf("expected success, got %v", rsp.ErrorCode)
	}
}

// 跨节点 BatchSend：只要有一条消息不可序列化就整批拒绝（不产生部分投递）。
func TestClusterBatchSendAbortsOnUnserializableMessage(t *testing.T) {
	s := newCodecSystem(t)
	cn := s.clusterNet
	remote := &vactor.ActorRefImpl{SystemId: 2, GroupSlot: 1, ActorType: ActorTypeStart + 30, ActorId: "b"}

	err := cn.Send(2, &vactor.EnvelopeBatchSend{
		ToActorRefs: []vactor.ActorRef{remote},
		// 第二条是 string，不是已注册的 proto 消息
		Messages: []interface{}{&protocol.Message{Type: 1}, "not-a-proto-message"},
	})
	if err == nil {
		t.Fatal("batch containing an unserializable message must be rejected")
	}
	if err.Code() != vactor.ErrorCodeCustomStart+1 { // ErrorCodeMessageCannotSerialize
		t.Fatalf("expected cannot-serialize code, got %v", err.Code())
	}

	// 全部可序列化时正常走到发送阶段（远端未连接 → 发送失败，但编码通过）
	err = cn.Send(2, &vactor.EnvelopeBatchSend{
		ToActorRefs: []vactor.ActorRef{remote},
		Messages:    []interface{}{&protocol.Message{Type: 1}},
	})
	if err != nil && err.Code() == vactor.ErrorCodeCustomStart+1 {
		t.Fatalf("valid batch should not be rejected at serialization: %v", err)
	}
}

// 类型解析缓存必须在覆盖注册后失效，否则会把消息编到旧的 msgType 上。
func TestRegisterMessageTypeInvalidatesCache(t *testing.T) {
	s := newCodecSystem(t)
	// 先走一次编解码，让缓存命中
	if _, err := s.MarshalMessage(&protocol.Message{Type: 1}); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	pkg1, err := s.MarshalMessage(&protocol.Message{Type: 1})
	if err != nil {
		t.Fatal(err)
	}
	if pkg1.Type != 1 {
		t.Fatalf("first registration should map to type 1, got %d", pkg1.Type)
	}
	// 重新注册到另一个 msgType
	s.RegisterMessageType(2, func() proto.Message { return &protocol.Message{} })
	pkg2, err := s.MarshalMessage(&protocol.Message{Type: 1})
	if err != nil {
		t.Fatal(err)
	}
	if pkg2.Type != 2 {
		t.Fatalf("cache not invalidated after re-register: type=%d, want 2", pkg2.Type)
	}
	// 反序列化侧同样要跟着走新注册
	if _, err := s.UnmarshalMessage(&protocol.Message{Type: 2, Data: nil}); err != nil {
		t.Fatalf("unmarshal with re-registered type: %v", err)
	}
}

// 序号类字段必须是 64 位：超过 uint32 的 RequestId/CallbackId 必须原样往返，
// 否则长跑进程在序号回绕后会张冠李戴。
func TestProtoIdFieldsAreWide(t *testing.T) {
	const wide = uint64(1)<<40 + 9

	req := &protocol.PkgEnvelopeRequest{RequestId: wide, Message: &protocol.Message{Type: 1}}
	data, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	gotReq := &protocol.PkgEnvelopeRequest{}
	if err := proto.Unmarshal(data, gotReq); err != nil {
		t.Fatal(err)
	}
	if gotReq.RequestId != wide {
		t.Fatalf("RequestId truncated: got %d, want %d", gotReq.RequestId, wide)
	}

	rsp := &protocol.PkgEnvelopeResponse{RequestId: wide, Response: &protocol.Response{}}
	data, err = proto.Marshal(rsp)
	if err != nil {
		t.Fatal(err)
	}
	gotRsp := &protocol.PkgEnvelopeResponse{}
	if err := proto.Unmarshal(data, gotRsp); err != nil {
		t.Fatal(err)
	}
	if gotRsp.RequestId != wide {
		t.Fatalf("RequestId truncated: got %d, want %d", gotRsp.RequestId, wide)
	}

	ara := &protocol.PkgEnvelopeResponseAsync{CallbackId: wide, CallbackAddress: wide, Response: &protocol.Response{}}
	data, err = proto.Marshal(ara)
	if err != nil {
		t.Fatal(err)
	}
	gotAra := &protocol.PkgEnvelopeResponseAsync{}
	if err := proto.Unmarshal(data, gotAra); err != nil {
		t.Fatal(err)
	}
	if gotAra.CallbackId != wide || gotAra.CallbackAddress != wide {
		t.Fatalf("async ids truncated: got %d/%d, want %d", gotAra.CallbackId, gotAra.CallbackAddress, wide)
	}
}

// vactor.CallbackId 必须是 64 位（编译期约束的行为体现）。
func TestCallbackIdIsWide(t *testing.T) {
	var id vactor.CallbackId = 1<<40 + 3
	if uint64(id) != 1<<40+3 {
		t.Fatalf("CallbackId lost precision: %d", uint64(id))
	}
}
