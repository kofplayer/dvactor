package dvactor_test

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/kofplayer/dvactor"
	dtu "github.com/kofplayer/dvactor/testutil"
	"github.com/kofplayer/vactor"
	vt "github.com/kofplayer/vactor/testutil"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// 配置校验：LocalSystemId 不在 SystemConfigs 中必须直接报错（原来会延迟到
// NewServer 时 nil 解引用 panic），重复 SystemId、多节点缺端口同样拒绝。
func TestNewSystemValidatesConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  *dvactor.ClusterConfig
	}{
		{
			name: "local id not in configs",
			cfg: &dvactor.ClusterConfig{
				LocalSystemId: 9,
				SystemConfigs: []*dvactor.SystemConfig{{SystemId: 1}},
			},
		},
		{
			name: "duplicate system id",
			cfg: &dvactor.ClusterConfig{
				LocalSystemId: 1,
				SystemConfigs: []*dvactor.SystemConfig{{SystemId: 1}, {SystemId: 1, Port: 1}},
			},
		},
		{
			name: "multi-node missing port",
			cfg: &dvactor.ClusterConfig{
				LocalSystemId: 1,
				SystemConfigs: []*dvactor.SystemConfig{{SystemId: 1, Port: 8001}, {SystemId: 2}},
			},
		},
		{
			name: "empty configs",
			cfg:  &dvactor.ClusterConfig{LocalSystemId: 1},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			dvactor.NewSystem(c.cfg)
		})
	}
	// 合法的单节点配置不应 panic
	dvactor.NewSystem(&dvactor.ClusterConfig{
		LocalSystemId: 1,
		SystemConfigs: []*dvactor.SystemConfig{{SystemId: 1}},
	})
}

// 监听端口被占：clusterServer.Start 立即返回错误并透传给 ClusterStartError，
// 不再被吞进后台日志。
func TestClusterStartReportsBindFailure(t *testing.T) {
	cfgs := dtu.BuildClusterConfig(t, 2, [][]vactor.ActorType{{clusterEchoType}, nil})
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfgs[0].Port))
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer func() { _ = ln.Close() }()

	n1ts, n1 := dtu.NewNode(t, 1, cfgs, 2*time.Second, nil)
	n1.Start()
	if n1.ClusterStartError() == nil {
		t.Fatal("expected ClusterStartError on bind failure")
	}
	if !n1ts.LogContains("cluster net start failed") {
		t.Fatal("expected start failure log")
	}
}

// 回归测试（report 2026-08-30 附录）：分区期间发出的 watch 订阅会静默丢失
// （watch 信封发送失败）。重连成功后 onSystemReconnected 触发 WatchProxy 刷新，
// 订阅自愈。本测试在分区窗口内订阅，重连后必须能收到通知。
func TestClusterWatchSubscriptionSurvivesPartition(t *testing.T) {
	notify := make(chan *vactor.MsgOnWatchMsg, 8)
	cl := dtu.StartCluster(t, []dtu.NodeSpec{
		{ // node1：server；echo + watcher（订阅在分区窗口内发起）
			// 注意：node1 不注册 Int32Value，以便 node2 发来的毒消息触发会话关闭
			ActorTypes: []vactor.ActorType{clusterEchoType, clusterWatcherType},
			Register: func(s dvactor.ClusterSystem) {
				registerEcho(s)
				s.RegisterActorType(clusterWatcherType, func() vactor.Actor {
					return func(ctx vactor.EnvelopeContext) {
						switch ctx.GetMessage().(type) {
						case string:
							ctx.Watch(ctx.CreateActorRef(clusterTargetType, "t1"), clusterWatchType)
						case *vactor.MsgOnWatchMsg:
							notify <- ctx.GetMessage().(*vactor.MsgOnWatchMsg)
						}
					}
				})
			},
		},
		{ // node2：client；watchee + 毒消息类型（Int32Value，node1 未注册）
			ActorTypes: []vactor.ActorType{clusterTargetType},
			Register: func(s dvactor.ClusterSystem) {
				registerMsgTypes(s)
				s.RegisterActorType(clusterTargetType, func() vactor.Actor {
					return func(ctx vactor.EnvelopeContext) {
						switch ctx.GetMessage().(type) {
						case string:
							ctx.Notify(clusterWatchType, newStrValMsg("after-partition"))
						}
					}
				})
			},
		},
	})
	n1, n2 := cl.Node(0), cl.Node(1)

	// 1) 制造分区：node2 发送 node1 无法反序列化的消息 → node1 关闭会话
	if _, err := n2.Request(n2.CreateActorRef(clusterEchoType, "e"), newStrValMsg("warm"), clusterTestTimeout); err != nil {
		t.Fatalf("warmup request: %v", err)
	}
	n2.Send(n2.CreateActorRef(clusterEchoType, "e"), newIntValMsg(1)) // 毒消息
	vt.WaitFor(t, 5*time.Second, "session dropped on node1", func() bool {
		return cl.NodeLogsContain(0, "disconnected")
	})

	// 2) 分区窗口内发起订阅：watch 信封发送失败、订阅静默丢失
	n1.Send(n1.CreateActorRef(clusterWatcherType, "w"), "watch")
	time.Sleep(200 * time.Millisecond)

	// 3) 重连后（约 5~6s）WatchProxy 刷新订阅；周期性触发通知直到收到
	stopFire := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopFire:
				return
			case <-ticker.C:
				n2.Send(n2.CreateActorRef(clusterTargetType, "t1"), "go")
			}
		}
	}()
	m := vt.WaitChan(t, notify, 15*time.Second, "notify after partition healed")
	close(stopFire)
	wg.Wait()
	mv, ok := m.Message.(*wrapperspb.StringValue)
	if !ok || mv.GetValue() != "after-partition" {
		t.Fatalf("message = %T %v", m.Message, m.Message)
	}
}

// 优雅停机：Stop 先关集群网络（终止重连循环、关闭监听与会话）再停 actor 层，
// 必须在有限时间内返回且可重复调用。
func TestClusterGracefulStop(t *testing.T) {
	cl := dtu.StartCluster(t, []dtu.NodeSpec{
		{ActorTypes: []vactor.ActorType{clusterEchoType}, Register: func(s dvactor.ClusterSystem) {
			registerMsgTypes(s)
			registerEcho(s)
		}},
		{ActorTypes: []vactor.ActorType{clusterEchoType}, Register: func(s dvactor.ClusterSystem) {
			registerMsgTypes(s)
			registerEcho(s)
		}},
	})
	n2 := cl.Node(1)
	if _, err := n2.Request(n2.CreateActorRef(clusterEchoType, "e"), newStrValMsg("hi"), clusterTestTimeout); err != nil {
		t.Fatalf("warmup: %v", err)
	}

	done := make(chan struct{})
	go func() {
		cl.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cluster Stop hung")
	}
	cl.Stop() // 幂等
}
