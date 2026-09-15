package dvactor

import (
	"testing"
	"time"

	"github.com/kofplayer/dvactor/protocol"
	"github.com/kofplayer/vactor"
	"google.golang.org/protobuf/proto"
)

// 同步请求跨节点编解码用的 actor 类型（需 >= dvactor.ActorTypeStart）。
const syncAddrActorType vactor.ActorType = ActorTypeStart + 30

// 编码路径：EnvelopeRequest/EnvelopeResponse 的 CallbackAddress 必须原样写入线协议。
// 若在 Send 中漏填，跨节点同步响应会退化为 CallbackAddress=0（无实例保护），
// 单看功能测试不会失败——这条测试专门守住该字段不被静默丢弃。
func TestClusterNetSendCarriesSyncCallbackAddress(t *testing.T) {
	s := newCodecSystem(t)
	cn := s.clusterNet
	fs := &fakeNetSession{}
	info := cn.systemInfos[2]
	info.lock.Lock()
	info.passive = false
	info.session = fs
	info.lock.Unlock()

	const addr = uint64(1)<<40 + 11
	self := func(id string) vactor.ActorRef {
		return &vactor.ActorRefImpl{SystemId: 1, GroupSlot: 1, ActorType: syncAddrActorType, ActorId: vactor.ActorId(id)}
	}
	remote := func(id string) vactor.ActorRef {
		return &vactor.ActorRefImpl{SystemId: 2, GroupSlot: 1, ActorType: syncAddrActorType, ActorId: vactor.ActorId(id)}
	}

	if err := cn.Send(2, &vactor.EnvelopeRequest{
		FromActorRef: self("a"), ToActorRef: remote("b"),
		Message: &protocol.Message{Type: 1}, RequestId: 9, CallbackAddress: addr,
	}); err != nil {
		t.Fatalf("send request: %v", err)
	}
	if err := cn.Send(2, &vactor.EnvelopeResponse{
		FromActorRef: self("a"), ToActorRef: remote("b"),
		Response:  &vactor.Response{Message: &protocol.Message{Type: 1}},
		RequestId: 9, CallbackAddress: addr,
	}); err != nil {
		t.Fatalf("send response: %v", err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.sent) != 2 {
		t.Fatalf("expected 2 frames, got %d", len(fs.sent))
	}
	for _, f := range fs.sent {
		switch protocol.PkgType(f.msgId) {
		case protocol.PkgType_PkgTypeEnvelopeRequest:
			p := &protocol.PkgEnvelopeRequest{}
			if err := proto.Unmarshal(f.data, p); err != nil {
				t.Fatal(err)
			}
			if p.CallbackAddress != addr || p.RequestId != 9 {
				t.Fatalf("request addr/id = %d/%d, want %d/9", p.CallbackAddress, p.RequestId, addr)
			}
		case protocol.PkgType_PkgTypeEnvelopeResponse:
			p := &protocol.PkgEnvelopeResponse{}
			if err := proto.Unmarshal(f.data, p); err != nil {
				t.Fatal(err)
			}
			if p.CallbackAddress != addr || p.RequestId != 9 {
				t.Fatalf("response addr/id = %d/%d, want %d/9", p.CallbackAddress, p.RequestId, addr)
			}
		default:
			t.Fatalf("unexpected msgId %d", f.msgId)
		}
	}
}

// 解码 + 回带：OnMessage 必须把 CallbackAddress 读进 EnvelopeRequest，
// 被请求 actor 响应时再原样写回 EnvelopeResponse 一起过网。
// 这里用一个本地 echo，把请求从线协议还原并响应，验证最终回包携带同一地址。
func TestClusterNetSyncCallbackAddressEchoedBack(t *testing.T) {
	s := NewSystem(&ClusterConfig{
		LocalSystemId: 1,
		SystemConfigs: []*SystemConfig{
			{SystemId: 1, Port: 19881, ActorTypes: []vactor.ActorType{syncAddrActorType}},
			{SystemId: 2, Host: "127.0.0.1", Port: 19882, ActorTypes: []vactor.ActorType{syncAddrActorType}},
		},
	}, func(sc *vactor.SystemConfig) {
		sc.LogFunc = func(vactor.LogLevel, string, ...interface{}) {}
	}).(*system)
	s.RegisterMessageType(1, func() proto.Message { return &protocol.Message{} })
	s.RegisterActorType(syncAddrActorType, func() vactor.Actor {
		return func(ctx vactor.EnvelopeContext) {
			if m, ok := ctx.GetMessage().(*protocol.Message); ok {
				ctx.Response(m, nil)
			}
		}
	})
	// 只启动 actor 层，不启动集群网络：回包走 clusterNet.Send，由 fakeNetSession 承接。
	s.System.Start()
	defer s.System.Stop()

	fs := &fakeNetSession{}
	info := s.clusterNet.systemInfos[2]
	info.lock.Lock()
	info.passive = false
	info.session = fs
	info.lock.Unlock()

	const addr = uint64(1)<<40 + 13
	data, err := proto.Marshal(&protocol.PkgEnvelopeRequest{
		FromActorRef:    &protocol.ActorRef{SystemId: 2, GroupSlot: 1, ActorType: uint32(syncAddrActorType), ActorId: "caller"},
		ToActorRef:      &protocol.ActorRef{SystemId: 1, GroupSlot: 1, ActorType: uint32(syncAddrActorType), ActorId: "echo"},
		Message:         &protocol.Message{Type: 1},
		RequestId:       21,
		CallbackAddress: addr,
	})
	if err != nil {
		t.Fatalf("marshal request pkg: %v", err)
	}
	if err := s.clusterNet.OnMessage(uint32(protocol.PkgType_PkgTypeEnvelopeRequest), data); err != nil {
		t.Fatalf("OnMessage(request): %v", err)
	}

	var got *protocol.PkgEnvelopeResponse
	deadline := time.Now().Add(3 * time.Second)
	for got == nil && time.Now().Before(deadline) {
		fs.mu.Lock()
		for _, f := range fs.sent {
			if protocol.PkgType(f.msgId) != protocol.PkgType_PkgTypeEnvelopeResponse {
				continue
			}
			p := &protocol.PkgEnvelopeResponse{}
			if err := proto.Unmarshal(f.data, p); err != nil {
				fs.mu.Unlock()
				t.Fatal(err)
			}
			got = p
			break
		}
		fs.mu.Unlock()
		if got == nil {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if got == nil {
		t.Fatal("no sync response encoded within timeout")
	}
	if got.CallbackAddress != addr {
		t.Fatalf("CallbackAddress not echoed: got %d, want %d", got.CallbackAddress, addr)
	}
	if got.RequestId != 21 {
		t.Fatalf("RequestId = %d, want 21", got.RequestId)
	}
}
