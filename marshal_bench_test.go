package dvactor

import (
	"testing"

	"github.com/kofplayer/vactor"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// BenchmarkMarshalMessage 测量跨节点发送路径上每条消息的类型解析 + 序列化开销。
func BenchmarkMarshalMessage(b *testing.B) {
	s := NewSystem(&ClusterConfig{
		LocalSystemId: 1,
		SystemConfigs: []*SystemConfig{{SystemId: 1, ActorTypes: nil}},
	}, func(sc *vactor.SystemConfig) {
		sc.LogFunc = func(vactor.LogLevel, string, ...interface{}) {}
	}).(*system)
	s.RegisterMessageType(1, func() proto.Message { return &wrapperspb.StringValue{} })

	msg := wrapperspb.String("hello-world-payload")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.MarshalMessage(msg); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkUnmarshalMessage 测量接收路径的类型查找 + 反序列化开销。
func BenchmarkUnmarshalMessage(b *testing.B) {
	s := NewSystem(&ClusterConfig{
		LocalSystemId: 1,
		SystemConfigs: []*SystemConfig{{SystemId: 1, ActorTypes: nil}},
	}, func(sc *vactor.SystemConfig) {
		sc.LogFunc = func(vactor.LogLevel, string, ...interface{}) {}
	}).(*system)
	s.RegisterMessageType(1, func() proto.Message { return &wrapperspb.StringValue{} })

	pkg, err := s.MarshalMessage(wrapperspb.String("hello-world-payload"))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.UnmarshalMessage(pkg); err != nil {
			b.Fatal(err)
		}
	}
}
