package dvactor

import (
	"fmt"
	"testing"
	"time"

	"github.com/kofplayer/dvactor/protocol"
	"github.com/kofplayer/vactor"
	vt "github.com/kofplayer/vactor/testutil"
	"google.golang.org/protobuf/proto"
)

// 本文件覆盖"信任边界"类回归：远端字节流解出来的引用、以及绕过公开 API 直接
// 投递信封的调用方，都不能让本节点崩进程或丢一整批消息。

const hardeningActorType = ActorTypeStart + 31

// hardeningSystem 启动一个单节点集群系统（不涉网：单节点既不监听也不外连），
// 注册一个收集消息的 actor 类型与一种可跨节点传输的载荷类型。
func hardeningSystem(t *testing.T, col *vt.Collector) (*system, *vt.LogBuffer) {
	t.Helper()
	logs := vt.NewLogBuffer()
	s := NewSystem(&ClusterConfig{
		LocalSystemId: 1,
		SystemConfigs: []*SystemConfig{
			{SystemId: 1, ActorTypes: []vactor.ActorType{hardeningActorType}},
		},
	}, func(sc *vactor.SystemConfig) {
		sc.GroupCount = 1 // 所有引用落同一组，才能验证"同批不被牵连"
		sc.TickInterval = 10 * time.Millisecond
		sc.LogFunc = logs.LogFunc
	}).(*system)
	s.RegisterActorType(hardeningActorType, col.Creator())
	s.RegisterMessageType(1, func() proto.Message { return &protocol.Message{} })
	s.Start()
	t.Cleanup(s.Stop)
	return s, logs
}

func hardeningPayload(t *testing.T) *protocol.Message {
	t.Helper()
	inner, err := proto.Marshal(&protocol.Message{Type: 7, Data: []byte("p")})
	if err != nil {
		t.Fatalf("build payload: %v", err)
	}
	return &protocol.Message{Type: 1, Data: inner}
}

// ActorRefFromProto 必须拒绝越界字段：线协议用 uint32 承载 SystemId/GroupSlot，
// Go 侧是 uint16，静默截断会把 65636 变成 100 而命中真实存在的系统。
func TestActorRefFromProtoValidatesRange(t *testing.T) {
	cases := []struct {
		name string
		in   *protocol.ActorRef
		ok   bool
	}{
		{"nil", nil, false},
		{"valid", &protocol.ActorRef{SystemId: 1, GroupSlot: 1, ActorType: uint32(hardeningActorType), ActorId: "a"}, true},
		{"systemId out of range", &protocol.ActorRef{SystemId: 1<<16 + 4, GroupSlot: 1}, false},
		{"systemId max uint32", &protocol.ActorRef{SystemId: 0xFFFFFFFF, GroupSlot: 1}, false},
		{"groupSlot out of range", &protocol.ActorRef{SystemId: 1, GroupSlot: 1<<16 + 4}, false},
		// proto3 的空 ActorRef 子消息就是这个形状：槽位 0 说明发送方根本没填
		{"groupSlot zero (empty submessage)", &protocol.ActorRef{SystemId: 1, GroupSlot: 0}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ActorRefFromProto(c.in)
			if c.ok && got == nil {
				t.Fatalf("valid ref rejected: %v", c.in)
			}
			if !c.ok && got != nil {
				t.Fatalf("invalid ref accepted, got %v", got)
			}
			if c.ok && (got.GetSystemId() != vactor.SystemId(c.in.SystemId) ||
				got.GetGroupSlot() != vactor.GroupSlot(c.in.GroupSlot) ||
				got.GetActorId() != vactor.ActorId(c.in.ActorId)) {
				t.Fatalf("round trip mismatch: %v vs %v", got, c.in)
			}
		})
	}
}

// localTarget 拒绝 SystemId 不是本节点的引用（配置漂移或对端故障），并要求记日志。
func TestLocalTargetRejectsForeignSystemId(t *testing.T) {
	col := &vt.Collector{}
	s, logs := hardeningSystem(t, col)

	foreign := &protocol.ActorRef{SystemId: 2, GroupSlot: 1, ActorType: uint32(hardeningActorType), ActorId: "x"}
	if ref := s.clusterNet.localTarget(foreign); ref != nil {
		t.Fatalf("foreign system target must be rejected, got %v", ref)
	}
	// 断言锁定 dvactor 自己的日志文本：vactor 层也有含 "!= local system" 的丢弃日志，
	// 用那段文字断言会在本层校验被移除时仍然通过（第一轮回退验证就是这么暴露的）。
	if !logs.Contains("cluster configs disagree") {
		t.Fatal("localTarget rejection must be logged by the dvactor layer itself")
	}
	if ref := s.clusterNet.localTarget(&protocol.ActorRef{
		SystemId: 1, GroupSlot: 1, ActorType: uint32(hardeningActorType), ActorId: "y"}); ref == nil {
		t.Fatal("local target must be accepted")
	}
}

// 关键回归（原缺陷：一条畸形包毒死本组当前一整批消息）：
// 远端包的 To.SystemId 不属于本节点时，只丢这一条，OnMessage 仍返回 nil
// （返回 error 会被 engine 当作断链信号），且不影响后续包。
func TestOnMessageForeignSystemIdDropsOnlyThatPacket(t *testing.T) {
	col := &vt.Collector{}
	s, logs := hardeningSystem(t, col)
	cn := s.clusterNet
	pmsg := hardeningPayload(t)
	data, err := proto.Marshal(&protocol.PkgEnvelopeSend{
		FromActorRef: &protocol.ActorRef{SystemId: 2, GroupSlot: 1, ActorType: uint32(hardeningActorType), ActorId: "remote"},
		ToActorRef:   &protocol.ActorRef{SystemId: 999, GroupSlot: 1, ActorType: uint32(hardeningActorType), ActorId: "ghost"},
		Message:      pmsg,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := cn.OnMessage(uint32(protocol.PkgType_PkgTypeEnvelopeSend), data); err != nil {
		t.Fatalf("malformed target must not escalate to a session error: %v", err)
	}
	vt.WaitFor(t, 2*time.Second, "foreign target logged by dvactor", func() bool {
		return logs.Contains("cluster configs disagree")
	})

	// 同一条链路上的正常包必须照常投递
	okData, err := proto.Marshal(&protocol.PkgEnvelopeSend{
		ToActorRef: &protocol.ActorRef{SystemId: 1, GroupSlot: 1, ActorType: uint32(hardeningActorType), ActorId: "ok"},
		Message:    pmsg,
	})
	if err != nil {
		t.Fatalf("marshal ok: %v", err)
	}
	if err := cn.OnMessage(uint32(protocol.PkgType_PkgTypeEnvelopeSend), okData); err != nil {
		t.Fatalf("valid packet: %v", err)
	}
	col.WaitForMessages(t, 1, 2*time.Second, "valid packet still delivered after a poisoned one")
}

// 关键回归：一个批量包里的非法收件人不得让同批的合法收件人一起丢。
func TestOnMessageBatchPoisonIsIsolated(t *testing.T) {
	col := &vt.Collector{}
	s, logs := hardeningSystem(t, col)
	pmsg := hardeningPayload(t)
	local := &protocol.ActorRef{SystemId: 1, GroupSlot: 1, ActorType: uint32(hardeningActorType), ActorId: "good"}
	poison := &protocol.ActorRef{SystemId: 42, GroupSlot: 1, ActorType: uint32(hardeningActorType), ActorId: "bad"}
	empty := &protocol.ActorRef{} // 远端填了个空引用

	data, err := proto.Marshal(&protocol.PkgEnvelopeBatchSend{
		ToActorRefs: []*protocol.ActorRef{poison, empty, local, local, local, local},
		Messages:    []*protocol.Message{pmsg},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := s.clusterNet.OnMessage(uint32(protocol.PkgType_PkgTypeEnvelopeBatchSend), data); err != nil {
		t.Fatalf("batch with invalid recipients must not error the session: %v", err)
	}
	// 4 个合法收件人各收 1 条：非法项只影响自己
	col.WaitForMessages(t, 4, 3*time.Second, "all valid recipients in a poisoned batch must still be served")
	if !logs.Contains("is invalid") && !logs.Contains("cluster configs disagree") {
		t.Fatal("invalid recipients must be logged by the dvactor layer itself")
	}
}

// probeHardeningRouterSystem 给"只碰路由、不碰网络"的用例提供一个两节点系统。
func probeHardeningRouterSystem(t *testing.T) *system {
	t.Helper()
	return NewSystem(&ClusterConfig{
		LocalSystemId:  1,
		ConnectTimeout: 3 * time.Second,
		SystemConfigs: []*SystemConfig{
			{SystemId: 1, Host: "127.0.0.1", Port: 19961, ActorTypes: []vactor.ActorType{hardeningActorType}},
			{SystemId: 2, Host: "127.0.0.1", Port: 19962, ActorTypes: []vactor.ActorType{hardeningActorType}},
		},
	}, func(sc *vactor.SystemConfig) { sc.LogFunc = func(vactor.LogLevel, string, ...interface{}) {} }).(*system)
}

// Router 必须像 vactor 的 LocalRouter 一样拦住 nil 目标：Router 常在业务 goroutine
// 或网络读 goroutine 上被调用，那里没有 recover，nil 解引用就是整个进程崩溃。
// 原缺陷即 `system.Send(nil, msg)` 在集群版崩、在单机版只是一行日志。
func TestRouterRejectsNilTargets(t *testing.T) {
	logs := vt.NewLogBuffer()
	sys := NewSystem(&ClusterConfig{
		LocalSystemId: 1,
		SystemConfigs: []*SystemConfig{{SystemId: 1, ActorTypes: []vactor.ActorType{hardeningActorType}}},
	}, func(sc *vactor.SystemConfig) { sc.LogFunc = logs.LogFunc }).(*system)

	envs := []vactor.Envelope{
		&vactor.EnvelopeSend{Message: "x"},
		&vactor.EnvelopeRequest{Message: "x"},
		&vactor.EnvelopeRequestAsync{Message: "x"},
		&vactor.EnvelopeResponse{Response: &vactor.Response{}},
		&vactor.EnvelopeResponseAsync{Response: &vactor.Response{}},
		&vactor.EnvelopeWatch{WatchType: 1, IsWatch: true},
		&vactor.EnvelopeOuterWatch{WatchType: 1, IsWatch: true},
		&vactor.EnvelopeOuterRequest{Message: "x"},
		&vactor.EnvelopeFireNotify{WatchType: 1},
		// 异系统目标 + 远程 watch（走 FromActorRef 分支）
		&vactor.EnvelopeWatch{
			ToActorRef:   &vactor.ActorRefImpl{SystemId: 2, GroupSlot: 1, ActorType: hardeningActorType, ActorId: "r"},
			WatchType:    1,
			IsWatch:      true,
			FromActorRef: nil,
		},
	}
	for i, env := range envs {
		t.Run(fmt.Sprintf("%T/%d", env, i), func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("router panicked on a missing target (case %d): %v", i, r)
				}
			}()
			if err := sys.router.Router(env); err == nil {
				t.Fatal("a missing target must be reported as an error, not silently accepted")
			}
		})
	}

	// 公开入口：Send/BatchSend/Watch 传 nil 引用都不能崩
	sys.Send(nil, "x")
	sys.Watch(nil, 1, vactor.NewQueue[interface{}]())
	if err := sys.BatchSend([]vactor.ActorRef{nil}, []interface{}{"x"}); err != nil {
		t.Fatalf("BatchSend with only nil refs: %v", err)
	}
}

// proxyActorId 与 GetWatcheeActorRef 是一对编解码，必须严格互逆：
// 代理 actor 的寻址全靠这个 id 反推出被代理者，配对出错的表现是"订阅静默丢失"。
func TestProxyActorIdRoundTrip(t *testing.T) {
	cases := []struct {
		actorType vactor.ActorType
		actorId   vactor.ActorId
	}{
		{hardeningActorType, "room"},
		// id 里带分隔符：解析按"第一个 -"切分，类型段不含 - 所以不歧义
		{hardeningActorType, "room-42-player"},
		{vactor.ActorType(12), "x"},
		{vactor.ActorType(1), "2-x"},
	}
	// GetWatcheeActorRef 通过 ctx.CreateActorRef 还原引用，所以要一个真上下文
	ctxCh := make(chan vactor.EnvelopeContext, 1)
	s := NewSystem(&ClusterConfig{
		LocalSystemId: 1,
		SystemConfigs: []*SystemConfig{{SystemId: 1, ActorTypes: []vactor.ActorType{hardeningActorType}}},
	}, func(sc *vactor.SystemConfig) {
		sc.GroupCount = 1
		sc.LogFunc = func(vactor.LogLevel, string, ...interface{}) {}
	}).(*system)
	s.RegisterActorType(hardeningActorType, func() vactor.Actor {
		return func(ctx vactor.EnvelopeContext) {
			select {
			case ctxCh <- ctx:
			default:
			}
		}
	})
	s.Start()
	defer s.Stop()
	s.Send(s.CreateActorRef(hardeningActorType, "probe"), "boot")
	var ctx vactor.EnvelopeContext
	select {
	case ctx = <-ctxCh:
	case <-time.After(3 * time.Second):
		t.Fatal("no EnvelopeContext captured")
	}

	for _, c := range cases {
		ref := &vactor.ActorRefImpl{ActorType: c.actorType, ActorId: c.actorId}
		encoded := proxyActorId(ref)
		decoded := GetWatcheeActorRef(ctx, encoded)
		if decoded == nil {
			t.Fatalf("id %q failed to decode back", encoded)
		}
		if decoded.GetActorType() != c.actorType || decoded.GetActorId() != c.actorId {
			t.Fatalf("round trip %v/%q -> %q -> %v/%q",
				c.actorType, c.actorId, encoded, decoded.GetActorType(), decoded.GetActorId())
		}
	}
}

// 分桶：容量精确（没有 append 倍增）、nil 被滤掉、未知 SystemId 仍然单独成桶
// （否则 ErrorCodeUnknownSystem 那条路径就丢了）。
func TestRouterSplitBySystem(t *testing.T) {
	s := probeHardeningRouterSystem(t)
	r := s.router
	local := func(id string) vactor.ActorRef {
		return &vactor.ActorRefImpl{SystemId: 1, GroupSlot: 1, ActorType: hardeningActorType, ActorId: vactor.ActorId(id)}
	}
	remote := func(id string) vactor.ActorRef {
		return &vactor.ActorRefImpl{SystemId: 2, GroupSlot: 1, ActorType: hardeningActorType, ActorId: vactor.ActorId(id)}
	}
	ghost := func(id string) vactor.ActorRef {
		return &vactor.ActorRefImpl{SystemId: 77, GroupSlot: 1, ActorType: hardeningActorType, ActorId: vactor.ActorId(id)}
	}
	refs := []vactor.ActorRef{local("a"), remote("b"), nil, local("c"), ghost("d"), remote("e"), nil}

	groups := r.splitBySystem(refs)
	if len(groups) != 3 {
		t.Fatalf("buckets = %d, want 3 (one per distinct SystemId)", len(groups))
	}
	for id, want := range map[vactor.SystemId]int{1: 2, 2: 2, 77: 1} {
		got := groups[id]
		if len(got) != want {
			t.Fatalf("bucket %d has %d refs, want %d", id, len(got), want)
		}
		if cap(got) != len(got) {
			t.Errorf("bucket %d cap %d != len %d: growth allocations were not avoided", id, cap(got), len(got))
		}
		for _, ref := range got {
			if ref == nil {
				t.Fatalf("bucket %d contains a nil ref", id)
			}
			if ref.GetSystemId() != id {
				t.Fatalf("ref %v landed in bucket %d", ref, id)
			}
		}
	}
	// 顺序必须保持输入序（notify 扇出的相对顺序靠它）
	if got := groups[1]; got[0].GetActorId() != "a" || got[1].GetActorId() != "c" {
		t.Fatalf("input order not preserved in bucket 1: %v", groups[1])
	}
}

// WatchProxy 的 watchee 解析失败（代理 actor id 畸形）时必须拒绝登记订阅。
// 放行 nil 目标的后果不是 panic 而是更糟的静默失效：本地记成"已回源订阅"，
// 而跨网那一步永远不会成功，重连刷新也补不回来。
func TestWatchProxyRejectsSubscriptionWithoutWatchee(t *testing.T) {
	col := &vt.Collector{}
	s, logs := hardeningSystem(t, col)
	proxy := &vactor.ActorRefImpl{
		SystemId: 1, GroupSlot: 1,
		ActorType: WatchProxyActorType,
		ActorId:   "not-a-encoded-watchee-id", // 首段不是数字 → GetWatcheeActorRef 返回 nil
	}
	// 与 Router 的远程 watch 分支同样的投递方式：InnerWatch 消息发给代理
	_ = s.LocalRouter(&vactor.EnvelopeSend{
		FromActorRef: s.CreateActorRef(hardeningActorType, "watcher"),
		ToActorRef:   proxy,
		Message:      &InnerWatch{WatchType: 5, IsWatch: true},
	})
	vt.WaitFor(t, 2*time.Second, "malformed proxy refused the subscription", func() bool {
		return logs.Contains("no valid watchee")
	})
	if logs.Contains("nil target actor") {
		t.Fatal("a nil watchee must not reach the router as a target")
	}
	// 系统仍然健康
	s.Send(s.CreateActorRef(hardeningActorType, "ok"), "x")
	col.WaitForMessages(t, 1, 2*time.Second, "system still serving after the malformed proxy request")
}
