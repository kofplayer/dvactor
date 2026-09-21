package dvactor_test

import (
	"context"
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
// 订阅自愈。本测试在分区窗口内订阅，对端起来后必须能收到通知。
//
// 分区用"对端节点晚启动"制造，而不是靠往对端塞一条解不开的消息：后者曾是唯一的
// 注入手段，但自那以后解码错误不再拆链（一条不兼容消息不该打断整条链路），
// 已不能再当分区发生器——晚启动是更真实的场景，且不需要为测试开生产 API。
func TestClusterWatchSubscriptionSurvivesPartition(t *testing.T) {
	notify := make(chan *vactor.MsgOnWatchMsg, 8)
	cfgs := dtu.BuildClusterConfig(t, 2, [][]vactor.ActorType{
		{clusterWatcherType}, // node1（下标 0，监听方）：watcher 在这里
		{clusterTargetType},  // node2（下标 1，主动外连）：watchee 在这里
	})

	// node1 先起：它要等 node2 注册才能凑齐全互联，用短超时让它先返回
	n1ts, n1 := dtu.NewNode(t, 1, cfgs, 1*time.Second, func(s dvactor.ClusterSystem) {
		registerMsgTypes(s) // 通知载荷是 StringValue，收发两侧都必须注册
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
	})
	t.Cleanup(n1.Stop)
	n1.Start()

	// 1) 分区窗口内发起订阅：node2 还没起来，watch 信封发送失败、订阅静默丢失
	n1.Send(n1.CreateActorRef(clusterWatcherType, "w"), "watch")
	time.Sleep(300 * time.Millisecond)

	// 2) node2 晚启动：它的 client 连上 node1 并注册 → node1 侧触发
	// onSystemReconnected → WatchProxy 重新发起订阅
	n2ts, n2 := dtu.NewNode(t, 2, cfgs, 8*time.Second, func(s dvactor.ClusterSystem) {
		registerMsgTypes(s)
		s.RegisterActorType(clusterTargetType, func() vactor.Actor {
			return func(ctx vactor.EnvelopeContext) {
				switch ctx.GetMessage().(type) {
				case string:
					ctx.Notify(clusterWatchType, newStrValMsg("after-partition"))
				}
			}
		})
	})
	t.Cleanup(n2.Stop)
	go n2.Start()

	// 3) 周期性触发通知，直到刷新后的订阅生效
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
	m := vt.WaitChan(t, notify, 20*time.Second, "notify after partition healed")
	// 自愈的前提是 node1 感知到了 node2 的接入；没有这一步，收到通知就是假通过
	if !n1ts.LogContains("system 2 connected") {
		t.Fatal("node1 never observed node2 registering: the reconnect path was not exercised")
	}
	if !n2ts.LogContains("system 1 connected") {
		t.Fatal("node2 never completed its outgoing registration")
	}
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

// StopWithContext 必须和 Stop 一样先关集群网络。内嵌的 vactor.System 实现只停
// actor 层——直接沿用会让重连循环与会话继续收发，等于一个还在对外收包的"已停机"
// 节点，所以 dvactor 必须覆盖这个方法。本用例锁住这一点。
func TestClusterStopWithContextAlsoStopsClusterNet(t *testing.T) {
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
	n1, n2 := cl.Node(0), cl.Node(1)
	if _, err := n2.Request(n2.CreateActorRef(clusterEchoType, "e"), newStrValMsg("hi"), clusterTestTimeout); err != nil {
		t.Fatalf("warmup: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := n1.StopWithContext(ctx); err != nil {
		t.Fatalf("StopWithContext: %v", err)
	}

	// 链路必须真的断掉：node2 是外连方，它会观察到 disconnect
	vt.WaitFor(t, 10*time.Second, "node2 observed the cluster link drop", func() bool {
		return cl.NodeLogsContain(1, "disconnected")
	})
	// 停机后不再有可用的跨节点请求（actor 层与网络层都已停）
	if _, err := n2.Request(n2.CreateActorRef(clusterEchoType, "e"), newStrValMsg("after"), 2*time.Second); err == nil {
		t.Fatal("cross-node request still succeeded against a stopped node")
	}
	// 幂等：再停一次不报错
	n1.Stop()
}
