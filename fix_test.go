package dvactor

import (
	"testing"

	netConnect "github.com/kofplayer/dvactor/engine/net/connect"
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
	if _, _, ok := sp.Next(); ok {
		t.Fatal("should not emit frame on partial header")
	}
	// 模拟粘包：剩余字节一次给齐（含两帧）
	sp.Append(stream[3:])

	id, payload, ok := sp.Next()
	if !ok || id != 1 || string(payload) != "hello" {
		t.Fatalf("frame1 mismatch: id=%v payload=%q ok=%v", id, payload, ok)
	}
	id, payload, ok = sp.Next()
	if !ok || id != 11 || string(payload) != "world!!" {
		t.Fatalf("frame2 mismatch: id=%v payload=%q ok=%v", id, payload, ok)
	}
	if _, _, ok = sp.Next(); ok {
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
