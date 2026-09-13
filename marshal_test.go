package dvactor_test

import (
	"testing"

	"github.com/kofplayer/dvactor"
	"github.com/kofplayer/dvactor/protocol"
	"github.com/kofplayer/vactor"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const strValMsgType = 9001

func newStrVal() proto.Message { return &wrapperspb.StringValue{} }

// codec 描述未导出 *system 上的消息编解码能力（ClusterSystem 接口未包含）。
type codec interface {
	MarshalMessage(interface{}) (*protocol.Message, vactor.VAError)
	UnmarshalMessage(*protocol.Message) (interface{}, error)
}

// newRegistrySystem 创建一个未启动的单节点 dvactor 系统（注册/编解码无需启动网络）。
func newRegistrySystem(t *testing.T) dvactor.ClusterSystem {
	t.Helper()
	return dvactor.NewSystem(&dvactor.ClusterConfig{
		LocalSystemId: 1,
		SystemConfigs: []*dvactor.SystemConfig{{SystemId: 1}},
	}, func(sc *vactor.SystemConfig) {
		sc.LogFunc = func(vactor.LogLevel, string, ...interface{}) {}
	})
}

func TestMarshalUnmarshalRoundtrip(t *testing.T) {
	s := newRegistrySystem(t)
	s.RegisterMessageType(strValMsgType, newStrVal)

	pkg, err := s.(codec).MarshalMessage(wrapperspb.String("hello"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if pkg.Type != strValMsgType {
		t.Fatalf("pkg type = %v", pkg.Type)
	}

	msg, uerr := s.(codec).UnmarshalMessage(pkg)
	if uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	got, ok := msg.(*wrapperspb.StringValue)
	if !ok || got.GetValue() != "hello" {
		t.Fatalf("roundtrip got %T %v", msg, msg)
	}
}

func TestMarshalUnregisteredType(t *testing.T) {
	s := newRegistrySystem(t)
	_, err := s.(codec).MarshalMessage(wrapperspb.String("x"))
	if err == nil || err.Code() != dvactor.ErrorCodeMessageNotRegister {
		t.Fatalf("expected ErrorCodeMessageNotRegister, got %v", err)
	}
}

func TestMarshalNonProtoMessage(t *testing.T) {
	s := newRegistrySystem(t)
	s.RegisterMessageType(strValMsgType, newStrVal)
	_, err := s.(codec).MarshalMessage("plain string")
	if err == nil || err.Code() != dvactor.ErrorCodeMessageCannotSerialize {
		t.Fatalf("expected ErrorCodeMessageCannotSerialize, got %v", err)
	}
}

func TestUnmarshalErrors(t *testing.T) {
	s := newRegistrySystem(t)
	s.RegisterMessageType(strValMsgType, newStrVal)

	tests := []struct {
		name string
		msg  *protocol.Message
	}{
		{"nil message", nil},
		{"corrupt payload", &protocol.Message{Type: strValMsgType, Data: []byte{1, 2}}},
		{"corrupt payload 2", &protocol.Message{Type: strValMsgType, Data: []byte{0, 0, 0, 0, 0xFF, 0xFF}}},
		{"unknown type", &protocol.Message{Type: 7777, Data: make([]byte, 8)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.(codec).UnmarshalMessage(tt.msg); err == nil {
				t.Fatalf("%s: expected error, got nil", tt.name)
			}
		})
	}
}

func TestDistributedErrorCodes(t *testing.T) {
	t.Parallel()

	// 错误码是线协议的一部分，数值漂移会破坏跨节点/跨语言互操作
	tests := []struct {
		name string
		code vactor.ErrorCode
		want vactor.ErrorCode
	}{
		{"MessageCannotSerialize", dvactor.ErrorCodeMessageCannotSerialize, 101},
		{"MessageNotRegister", dvactor.ErrorCodeMessageNotRegister, 102},
		{"MessageSerializeFail", dvactor.ErrorCodeMessageSerializeFail, 103},
		{"MessageLenError", dvactor.ErrorCodeMessageLenError, 104},
		{"UnknownEnvelope", dvactor.ErrorCodeUnknownEnvelope, 105},
		{"MessageSendFail", dvactor.ErrorCodeMessageSendFail, 106},
		{"CustomStart", dvactor.ErrorCodeCustomStart, 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.code != tt.want {
				t.Fatalf("error code drift: got %d, want %d", tt.code, tt.want)
			}
		})
	}
}
