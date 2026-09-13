package dvactor_test

import (
	"sync"
	"testing"
	"time"

	"github.com/kofplayer/dvactor"
	dtu "github.com/kofplayer/dvactor/testutil"
	"github.com/kofplayer/vactor"
	vt "github.com/kofplayer/vactor/testutil"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// 配置了共享密钥且两侧一致：注册成功，集群正常工作。
func TestClusterAuthTokenAccepted(t *testing.T) {
	cl := dtu.StartCluster(t, []dtu.NodeSpec{
		{ActorTypes: []vactor.ActorType{clusterEchoType}, Register: func(s dvactor.ClusterSystem) {
			registerMsgTypes(s)
			registerEcho(s)
		}},
		{ActorTypes: []vactor.ActorType{clusterEchoType}, Register: func(s dvactor.ClusterSystem) {
			registerMsgTypes(s)
			registerEcho(s)
		}},
	}, dtu.WithAuthToken("shared-secret"))

	n1 := cl.Node(0)
	rsp, err := n1.Request(n1.CreateActorRef(clusterEchoType, "e"), newStrValMsg("ping"), clusterTestTimeout)
	if err != nil {
		t.Fatalf("request with valid token: %v", err)
	}
	if rsp.(*wrapperspb.StringValue).GetValue() != "echo:ping" {
		t.Fatalf("rsp = %v", rsp)
	}
}

// token 不匹配：server 拒绝注册并关闭会话；client 无法接入，
// ConnectTimeout 后 ClusterStartError 非空，server 记录鉴权失败日志。
func TestClusterAuthTokenRejected(t *testing.T) {
	cfgs := dtu.BuildClusterConfig(t, 2, [][]vactor.ActorType{{clusterEchoType}, {clusterEchoType}})
	n1ts, n1 := dtu.NewNode(t, 1, cfgs, 2*time.Second, func(s dvactor.ClusterSystem) {
		registerMsgTypes(s)
		registerEcho(s)
	}, dtu.WithAuthToken("right-token"))
	_, n2 := dtu.NewNode(t, 2, cfgs, 2*time.Second, func(s dvactor.ClusterSystem) {
		registerMsgTypes(s)
		registerEcho(s)
	}, dtu.WithAuthToken("wrong-token"))

	var wg sync.WaitGroup
	wg.Add(2)
	for _, s := range []dvactor.ClusterSystem{n1, n2} {
		go func(s dvactor.ClusterSystem) {
			defer wg.Done()
			s.Start()
		}(s)
	}
	wg.Wait()

	if n2.ClusterStartError() == nil {
		t.Fatal("node with wrong token must fail to join the cluster")
	}
	vt.WaitFor(t, 3*time.Second, "server logged auth rejection", func() bool {
		return n1ts.LogContains("auth token mismatch")
	})
	// 合法节点不受影响……本用例只有两个节点，被拒节点未接入，
	// 但 node1 的 server 持续存活（监听未受影响）
	if n1.ClusterStartError() == nil {
		// node1 也在等 node2 注册，ConnectTimeout 后同样报错——属预期行为
		t.Log("node1 start error (timeout waiting for rejected node2):", n1.ClusterStartError())
	}
}

// Message.Data 去除 4 字节 msgType 前缀后：空载荷消息的编解码往返。
func TestMarshalEmptyPayloadRoundtrip(t *testing.T) {
	s := newRegistrySystem(t)
	s.RegisterMessageType(strValMsgType, newStrVal)

	pkg, err := s.(codec).MarshalMessage(&wrapperspb.StringValue{})
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if len(pkg.Data) != 0 {
		t.Fatalf("empty message data length = %d, want 0 (no legacy 4-byte prefix)", len(pkg.Data))
	}
	msg, uerr := s.(codec).UnmarshalMessage(pkg)
	if uerr != nil {
		t.Fatalf("unmarshal empty: %v", uerr)
	}
	if _, ok := msg.(*wrapperspb.StringValue); !ok {
		t.Fatalf("roundtrip got %T", msg)
	}
}
