package dvactor_test

import (
	"sync"
	"testing"
	"time"

	"github.com/kofplayer/dvactor"
	dtu "github.com/kofplayer/dvactor/testutil"
	"github.com/kofplayer/vactor"
	vt "github.com/kofplayer/vactor/testutil"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	clusterEchoType     = dvactor.ActorTypeStart + 100 // 目标/echo actor
	clusterWatcherType  = dvactor.ActorTypeStart + 101 // 跨节点 watcher
	clusterTargetType   = dvactor.ActorTypeStart + 102 // 跨节点 watchee
	clusterListenerType = dvactor.ActorTypeStart + 103 // 跨节点事件监听
	clusterLocalType    = dvactor.ActorTypeStart + 104 // 本机消息（不序列化）
)

const (
	clusterStrMsgType  = uint32(9001)
	clusterIntMsgType  = uint32(9002)
	clusterWatchType   = vactor.WatchType(77)
	clusterEventGroup  = vactor.EventGroup("orders")
	clusterEventID     = vactor.EventId(9)
	clusterTestTimeout = 5 * time.Second
)

func newStrValMsg(v string) *wrapperspb.StringValue { return wrapperspb.String(v) }
func newIntValMsg(v int32) *wrapperspb.Int32Value   { return wrapperspb.Int32(v) }

func strValCreator() proto.Message { return &wrapperspb.StringValue{} }
func intValCreator() proto.Message { return &wrapperspb.Int32Value{} }

// registerMsgTypes 在节点上注册全部测试消息类型。
// dvactor 约束：每个节点都必须注册自己可能收发的全部消息类型。
func registerMsgTypes(s dvactor.ClusterSystem) {
	s.RegisterMessageType(clusterStrMsgType, strValCreator)
	s.RegisterMessageType(clusterIntMsgType, intValCreator)
}

// registerEcho 注册跨节点 echo actor：StringValue 请求 → "echo:<v>" 响应。
// "noresp" 不响应（超时测试用）。
func registerEcho(s dvactor.ClusterSystem) {
	s.RegisterActorType(clusterEchoType, func() vactor.Actor {
		return func(ctx vactor.EnvelopeContext) {
			switch m := ctx.GetMessage().(type) {
			case *wrapperspb.StringValue:
				if m.GetValue() == "noresp" {
					return
				}
				ctx.Response(newStrValMsg("echo:"+m.GetValue()), nil)
			}
		}
	})
	s.RegisterMessageType(clusterStrMsgType, strValCreator)
}

// 跨节点 Send：node1 发往 node2 上的收集 actor。
func TestClusterSendAcrossNodes(t *testing.T) {
	col := &vt.Collector{}
	cl := dtu.StartCluster(t, []dtu.NodeSpec{
		{ActorTypes: []vactor.ActorType{clusterLocalType}, Register: registerMsgTypes},
		{ActorTypes: []vactor.ActorType{clusterTargetType}, Register: func(s dvactor.ClusterSystem) {
			registerMsgTypes(s)
			s.RegisterActorType(clusterTargetType, col.Creator())
		}},
	})
	n1 := cl.Node(0)

	n1.Send(n1.CreateActorRef(clusterTargetType, "t1"), newStrValMsg("hello"))
	msgs := col.WaitForMessages(t, 1, clusterTestTimeout, "cross-node send")
	if m, ok := msgs[0].(*wrapperspb.StringValue); !ok || m.GetValue() != "hello" {
		t.Fatalf("got %T %v", msgs[0], msgs[0])
	}
}

// 跨节点同步 Request（外部调用方 → RequestProxy → 远程 echo）。
func TestClusterRequestRoundtrip(t *testing.T) {
	cl := dtu.StartCluster(t, []dtu.NodeSpec{
		{ActorTypes: []vactor.ActorType{clusterLocalType}, Register: registerMsgTypes},
		{ActorTypes: []vactor.ActorType{clusterEchoType}, Register: func(s dvactor.ClusterSystem) {
			registerMsgTypes(s)
			registerEcho(s)
		}},
	})
	n1 := cl.Node(0)

	rsp, err := n1.Request(n1.CreateActorRef(clusterEchoType, "e1"), newStrValMsg("ping"), clusterTestTimeout)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if m, ok := rsp.(*wrapperspb.StringValue); !ok || m.GetValue() != "echo:ping" {
		t.Fatalf("got %T %v", rsp, rsp)
	}
}

// 跨节点 actor 间异步请求：node1 上的 caller 向 node2 上的 echo 发 RequestAsync。
func TestClusterActorAsyncRequest(t *testing.T) {
	result := make(chan string, 8)
	cl := dtu.StartCluster(t, []dtu.NodeSpec{
		{ActorTypes: []vactor.ActorType{clusterWatcherType}, Register: func(s dvactor.ClusterSystem) {
			s.RegisterMessageType(clusterStrMsgType, strValCreator)
			s.RegisterActorType(clusterWatcherType, func() vactor.Actor {
				return func(ctx vactor.EnvelopeContext) {
					switch ctx.GetMessage().(type) {
					case *vactor.MsgOnStart:
						ctx.RequestAsync(ctx.CreateActorRef(clusterEchoType, "e1"),
							newStrValMsg("async"), clusterTestTimeout,
							func(msg interface{}, err vactor.VAError) {
								if err != nil {
									result <- "ERR:" + err.Error()
								} else {
									result <- msg.(*wrapperspb.StringValue).GetValue()
								}
							})
					}
				}
			})
		}},
		{ActorTypes: []vactor.ActorType{clusterEchoType}, Register: func(s dvactor.ClusterSystem) {
			registerMsgTypes(s)
			registerEcho(s)
		}},
	})
	cl.Node(0).Send(cl.Node(0).CreateActorRef(clusterWatcherType, "w"), "boot")
	if got := vt.WaitChan(t, result, clusterTestTimeout, "async response"); got != "echo:async" {
		t.Fatalf("got %q", got)
	}
}

// 已知缺陷回归占位：跨节点"只回错误"的响应在发送侧被丢弃（见
// reports/code-review-2026-08-30.md 第一节）。修复后移除 Skip 即可用本测试验证。
func TestClusterErrorOnlyResponse(t *testing.T) {
	errSeen := make(chan vactor.ErrorCode, 8)
	cl := dtu.StartCluster(t, []dtu.NodeSpec{
		{ActorTypes: []vactor.ActorType{clusterWatcherType}, Register: func(s dvactor.ClusterSystem) {
			s.RegisterMessageType(clusterStrMsgType, strValCreator)
			s.RegisterActorType(clusterWatcherType, func() vactor.Actor {
				return func(ctx vactor.EnvelopeContext) {
					switch ctx.GetMessage().(type) {
					case *vactor.MsgOnStart:
						ctx.RequestAsync(ctx.CreateActorRef(clusterEchoType, "e1"),
							newStrValMsg("want-error"), clusterTestTimeout,
							func(msg interface{}, err vactor.VAError) {
								if err != nil {
									errSeen <- err.Code()
								} else {
									errSeen <- 0
								}
							})
					}
				}
			})
		}},
		{ActorTypes: []vactor.ActorType{clusterEchoType}, Register: func(s dvactor.ClusterSystem) {
			registerMsgTypes(s)
			s.RegisterActorType(clusterEchoType, func() vactor.Actor {
				return func(ctx vactor.EnvelopeContext) {
					switch ctx.GetMessage().(type) {
					case *wrapperspb.StringValue:
						// 只回错误，不带 payload
						ctx.Response(nil, vactor.NewVAError(123))
					}
				}
			})
		}},
	})
	cl.Node(0).Send(cl.Node(0).CreateActorRef(clusterWatcherType, "w"), "boot")
	if code := vt.WaitChan(t, errSeen, clusterTestTimeout, "error response"); code != 123 {
		t.Fatalf("expected error code 123, got %v", code)
	}
}

// 跨节点 actor 间 Watch：watcher 在 node1，watchee 在 node2（经 WatchProxy）。
func TestClusterInnerWatchCrossNode(t *testing.T) {
	notify := make(chan *vactor.MsgOnWatchMsg, 8)
	cl := dtu.StartCluster(t, []dtu.NodeSpec{
		{ActorTypes: []vactor.ActorType{clusterWatcherType}, Register: func(s dvactor.ClusterSystem) {
			s.RegisterMessageType(clusterStrMsgType, strValCreator)
			s.RegisterActorType(clusterWatcherType, func() vactor.Actor {
				return func(ctx vactor.EnvelopeContext) {
					switch m := ctx.GetMessage().(type) {
					case *vactor.MsgOnStart:
						ctx.Watch(ctx.CreateActorRef(clusterTargetType, "t1"), clusterWatchType)
					case *vactor.MsgOnWatchMsg:
						notify <- m
					case string:
						if m == "unwatch" {
							ctx.Unwatch(ctx.CreateActorRef(clusterTargetType, "t1"), clusterWatchType)
						}
					}
				}
			})
		}},
		{ActorTypes: []vactor.ActorType{clusterTargetType}, Register: func(s dvactor.ClusterSystem) {
			registerMsgTypes(s)
			s.RegisterActorType(clusterTargetType, func() vactor.Actor {
				return func(ctx vactor.EnvelopeContext) {
					switch ctx.GetMessage().(type) {
					case string:
						ctx.Notify(clusterWatchType, newStrValMsg("hi"))
					}
				}
			})
		}},
	})
	n1, n2 := cl.Node(0), cl.Node(1)

	n1.Send(n1.CreateActorRef(clusterWatcherType, "w"), "boot")
	// 等 watch 链路建立（含跨网）：重试投递直到收到通知，不依赖固定 sleep
	// ——固定等待在负载下（并行跑多包测试 / -race）会不够而假失败。
	var m *vactor.MsgOnWatchMsg
	deadline := time.Now().Add(clusterTestTimeout)
	for m == nil {
		n2.Send(n2.CreateActorRef(clusterTargetType, "t1"), "go")
		select {
		case m = <-notify:
		case <-time.After(200 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("cross-node watch notify not received within timeout")
			}
		}
	}
	// 丢弃重试期间产生的多余通知，保证后续 NoReceive 断言可靠
	for drained := false; !drained; {
		select {
		case <-notify:
		default:
			drained = true
		}
	}
	if m.WatchType != clusterWatchType {
		t.Fatalf("watchType = %v", m.WatchType)
	}
	if m.ActorRef.GetActorId() != "t1" {
		t.Fatalf("ActorRef should be the watchee, got %v", m.ActorRef.GetActorId())
	}
	if mv, ok := m.Message.(*wrapperspb.StringValue); !ok || mv.GetValue() != "hi" {
		t.Fatalf("message = %T %v", m.Message, m.Message)
	}

	// 跨节点 Unwatch 停止投递
	n1.Send(n1.CreateActorRef(clusterWatcherType, "w"), "unwatch")
	time.Sleep(400 * time.Millisecond)
	n2.Send(n2.CreateActorRef(clusterTargetType, "t1"), "go")
	vt.NoReceive(t, notify, time.Second, "notify after cross-node unwatch")
}

// 跨节点外部 Queue Watch：Queue 在 node1，watchee 在 node2。
func TestClusterOuterWatchCrossNode(t *testing.T) {
	queue := vactor.NewQueue[interface{}]()
	seen := make(chan *vactor.MsgOnWatchMsg, 8)
	cl := dtu.StartCluster(t, []dtu.NodeSpec{
		{ActorTypes: []vactor.ActorType{clusterLocalType}, Register: registerMsgTypes},
		{ActorTypes: []vactor.ActorType{clusterTargetType}, Register: func(s dvactor.ClusterSystem) {
			registerMsgTypes(s)
			s.RegisterActorType(clusterTargetType, func() vactor.Actor {
				return func(ctx vactor.EnvelopeContext) {
					switch ctx.GetMessage().(type) {
					case string:
						ctx.Notify(clusterWatchType, newStrValMsg("hi"))
					}
				}
			})
		}},
	})
	n1, n2 := cl.Node(0), cl.Node(1)

	n1.Watch(n1.CreateActorRef(clusterTargetType, "t1"), clusterWatchType, queue)
	go func() {
		for {
			m, ok := queue.Dequeue()
			if !ok {
				return
			}
			if msg, ok := m.(*vactor.MsgOnWatchMsg); ok {
				seen <- msg
			}
		}
	}()
	// 等 watch 链路建立：重试投递直到收到通知（理由同 TestClusterInnerWatchCrossNode）
	var m *vactor.MsgOnWatchMsg
	deadline := time.Now().Add(clusterTestTimeout)
	for m == nil {
		n2.Send(n2.CreateActorRef(clusterTargetType, "t1"), "go")
		select {
		case m = <-seen:
		case <-time.After(200 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("outer watch across nodes: no notify within timeout")
			}
		}
	}
	// 丢弃重试期间产生的多余通知，保证后续 NoReceive 断言可靠
	for drained := false; !drained; {
		select {
		case <-seen:
		default:
			drained = true
		}
	}
	if mv, ok := m.Message.(*wrapperspb.StringValue); !ok || mv.GetValue() != "hi" {
		t.Fatalf("message = %T %v", m.Message, m.Message)
	}

	n1.Unwatch(n1.CreateActorRef(clusterTargetType, "t1"), clusterWatchType, queue)
	time.Sleep(400 * time.Millisecond)
	n2.Send(n2.CreateActorRef(clusterTargetType, "t1"), "go")
	vt.NoReceive(t, seen, time.Second, "notify after outer unwatch")
}

// 同一节点上多个 watcher 聚合到一个 WatchProxy，watchee 只通知一次，双方都收到。
func TestClusterWatchProxyAggregation(t *testing.T) {
	n1c := make(chan *vactor.MsgOnWatchMsg, 8)
	n2c := make(chan *vactor.MsgOnWatchMsg, 8)
	cl := dtu.StartCluster(t, []dtu.NodeSpec{
		{ActorTypes: []vactor.ActorType{clusterWatcherType}, Register: func(s dvactor.ClusterSystem) {
			s.RegisterMessageType(clusterStrMsgType, strValCreator)
			s.RegisterActorType(clusterWatcherType, func() vactor.Actor {
				return func(ctx vactor.EnvelopeContext) {
					switch ctx.GetMessage().(type) {
					case *vactor.MsgOnStart:
						ctx.Watch(ctx.CreateActorRef(clusterTargetType, "t1"), clusterWatchType)
					case *vactor.MsgOnWatchMsg:
						ch := n1c
						if ctx.GetActorRef().GetActorId() == "w2" {
							ch = n2c
						}
						ch <- ctx.GetMessage().(*vactor.MsgOnWatchMsg)
					}
				}
			})
		}},
		{ActorTypes: []vactor.ActorType{clusterTargetType}, Register: func(s dvactor.ClusterSystem) {
			registerMsgTypes(s)
			s.RegisterActorType(clusterTargetType, func() vactor.Actor {
				return func(ctx vactor.EnvelopeContext) {
					switch ctx.GetMessage().(type) {
					case string:
						ctx.Notify(clusterWatchType, newStrValMsg("hi"))
					}
				}
			})
		}},
	})
	n1, n2 := cl.Node(0), cl.Node(1)
	n1.Send(n1.CreateActorRef(clusterWatcherType, "w1"), "boot")
	n1.Send(n1.CreateActorRef(clusterWatcherType, "w2"), "boot")
	time.Sleep(300 * time.Millisecond)
	n2.Send(n2.CreateActorRef(clusterTargetType, "t1"), "go")

	vt.WaitChan(t, n1c, clusterTestTimeout, "watcher w1")
	vt.WaitChan(t, n2c, clusterTestTimeout, "watcher w2")
}

// 跨节点事件：node1 监听，node2 触发（EventHub 需在两节点都声明）。
func TestClusterEventCrossNode(t *testing.T) {
	events := make(chan *vactor.MsgOnEventMsg, 8)
	cl := dtu.StartCluster(t, []dtu.NodeSpec{
		{ActorTypes: []vactor.ActorType{vactor.EventHubActorType, clusterListenerType},
			Register: func(s dvactor.ClusterSystem) {
				registerMsgTypes(s)
				s.RegisterActorType(clusterListenerType, func() vactor.Actor {
					return func(ctx vactor.EnvelopeContext) {
						switch m := ctx.GetMessage().(type) {
						case *vactor.MsgOnStart:
							ctx.ListenEvent(clusterEventGroup, clusterEventID)
						case *vactor.MsgOnEventMsg:
							events <- m
						}
					}
				})
			}},
		{ActorTypes: []vactor.ActorType{vactor.EventHubActorType},
			Register: registerMsgTypes},
	})
	cl.Node(0).Send(cl.Node(0).CreateActorRef(clusterListenerType, "l"), "boot")
	time.Sleep(300 * time.Millisecond)

	cl.Node(1).FireEvent(clusterEventGroup, clusterEventID, newStrValMsg("evt"))
	m := vt.WaitChan(t, events, clusterTestTimeout, "cross-node event")
	if m.EventGroup != clusterEventGroup || m.EventId != clusterEventID {
		t.Fatalf("fields wrong: %v/%v", m.EventGroup, m.EventId)
	}
	if mv, ok := m.Message.(*wrapperspb.StringValue); !ok || mv.GetValue() != "evt" {
		t.Fatalf("message = %T %v", m.Message, m.Message)
	}
}

// 跨节点事件顺序：同 EventGroup 严格有序。
func TestClusterEventOrdering(t *testing.T) {
	events := make(chan *vactor.MsgOnEventMsg, 128)
	cl := dtu.StartCluster(t, []dtu.NodeSpec{
		{ActorTypes: []vactor.ActorType{vactor.EventHubActorType, clusterListenerType},
			Register: func(s dvactor.ClusterSystem) {
				registerMsgTypes(s)
				s.RegisterActorType(clusterListenerType, func() vactor.Actor {
					return func(ctx vactor.EnvelopeContext) {
						switch m := ctx.GetMessage().(type) {
						case *vactor.MsgOnStart:
							ctx.ListenEvent(clusterEventGroup, clusterEventID)
						case *vactor.MsgOnEventMsg:
							events <- m
						}
					}
				})
			}},
		{ActorTypes: []vactor.ActorType{vactor.EventHubActorType},
			Register: registerMsgTypes},
	})
	cl.Node(0).Send(cl.Node(0).CreateActorRef(clusterListenerType, "l"), "boot")
	time.Sleep(300 * time.Millisecond)

	const n = 30
	for i := 0; i < n; i++ {
		cl.Node(1).FireEvent(clusterEventGroup, clusterEventID, newIntValMsg(int32(i)))
	}
	var got []int32
	for len(got) < n {
		m := vt.WaitChan(t, events, clusterTestTimeout, "event in order")
		got = append(got, m.Message.(*wrapperspb.Int32Value).GetValue())
	}
	for i, v := range got {
		if v != int32(i) {
			t.Fatalf("event order broken: %v", got)
		}
	}
}

// 跨节点消息顺序：单连接 FIFO，200 条按序到达。
func TestClusterSendOrderPreserved(t *testing.T) {
	var mu sync.Mutex
	var got []int32
	col := &vt.Collector{}
	cl := dtu.StartCluster(t, []dtu.NodeSpec{
		{ActorTypes: []vactor.ActorType{clusterLocalType}, Register: registerMsgTypes},
		{ActorTypes: []vactor.ActorType{clusterTargetType}, Register: func(s dvactor.ClusterSystem) {
			registerMsgTypes(s)
			s.RegisterActorType(clusterTargetType, func() vactor.Actor {
				return func(ctx vactor.EnvelopeContext) {
					if m, ok := ctx.GetMessage().(*wrapperspb.Int32Value); ok {
						mu.Lock()
						got = append(got, m.GetValue())
						mu.Unlock()
					}
					col.Observe(ctx)
				}
			})
		}},
	})
	n1 := cl.Node(0)
	const n = 200
	for i := 0; i < n; i++ {
		n1.Send(n1.CreateActorRef(clusterTargetType, "t"), newIntValMsg(int32(i)))
	}
	col.WaitForMessages(t, n, clusterTestTimeout, "all messages delivered")
	for i, v := range got {
		if v != int32(i) {
			t.Fatalf("order broken at %d: %v", i, got[:i+3])
		}
	}
}

// 集群运行期间，纯本机消息（非 proto、不序列化）不受影响。
func TestClusterLocalMessagesStillWork(t *testing.T) {
	col := &vt.Collector{}
	cl := dtu.StartCluster(t, []dtu.NodeSpec{
		{ActorTypes: []vactor.ActorType{clusterLocalType}, Register: func(s dvactor.ClusterSystem) {
			s.RegisterActorType(clusterLocalType, col.Creator())
		}},
		{ActorTypes: nil},
	})
	cl.Node(0).Send(cl.Node(0).CreateActorRef(clusterLocalType, "a"), 12345) // int 非法跨节点，本机可用
	msgs := col.WaitForMessages(t, 1, clusterTestTimeout, "local message")
	if msgs[0].(int) != 12345 {
		t.Fatalf("got %v", msgs[0])
	}
}

// 三节点全互联：1→3 发送、3→1 发送、2→3 请求。
func TestClusterThreeNodesMesh(t *testing.T) {
	col := &vt.Collector{}
	cl := dtu.StartCluster(t, []dtu.NodeSpec{
		{ActorTypes: []vactor.ActorType{clusterLocalType}, Register: func(s dvactor.ClusterSystem) {
			registerMsgTypes(s)
			s.RegisterActorType(clusterLocalType, col.Creator())
		}},
		{ActorTypes: nil, Register: registerMsgTypes},
		{ActorTypes: []vactor.ActorType{clusterEchoType}, Register: func(s dvactor.ClusterSystem) {
			registerMsgTypes(s)
			registerEcho(s)
		}},
	})
	n1, n2, n3 := cl.Node(0), cl.Node(1), cl.Node(2)

	// 1 → 3
	rsp, err := n1.Request(n1.CreateActorRef(clusterEchoType, "e"), newStrValMsg("m13"), clusterTestTimeout)
	if err != nil || rsp.(*wrapperspb.StringValue).GetValue() != "echo:m13" {
		t.Fatalf("1->3 request: (%v,%v)", rsp, err)
	}
	// 2 → 3
	rsp, err = n2.Request(n2.CreateActorRef(clusterEchoType, "e"), newStrValMsg("m23"), clusterTestTimeout)
	if err != nil || rsp.(*wrapperspb.StringValue).GetValue() != "echo:m23" {
		t.Fatalf("2->3 request: (%v,%v)", rsp, err)
	}
	// 3 → 1
	n3.Send(n3.CreateActorRef(clusterLocalType, "a"), newStrValMsg("m31"))
	col.WaitForMessages(t, 1, clusterTestTimeout, "3->1 send")
}

// 幽灵节点（配置了但未启动）：Start 按 ConnectTimeout 返回（错误被吞、仅记日志），
// 向幽灵节点发送快速失败 ErrorCodeMessageSendFail。
func TestClusterUnreachableNodeSendFail(t *testing.T) {
	actorTypes := [][]vactor.ActorType{
		{clusterLocalType},
		{clusterEchoType},
		{clusterEchoType}, // 节点 3 不启动
	}
	cfgs := dtu.BuildClusterConfig(t, 3, actorTypes)
	n1ts, n1 := dtu.NewNode(t, 1, cfgs, 2*time.Second, func(s dvactor.ClusterSystem) {
		registerEcho(s)
	})
	_, n2 := dtu.NewNode(t, 2, cfgs, 2*time.Second, func(s dvactor.ClusterSystem) {
		registerEcho(s)
	})
	var wg sync.WaitGroup
	wg.Add(2)
	for _, s := range []dvactor.ClusterSystem{n1, n2} {
		go func(s dvactor.ClusterSystem) {
			defer wg.Done()
			s.Start()
		}(s)
	}
	wg.Wait()

	if !n1ts.LogContains("cluster net start failed") {
		t.Fatal("expected start failure log for unreachable node 3")
	}
	if n1.ClusterStartError() == nil {
		t.Fatal("ClusterStartError should be exposed to the caller")
	}

	// 向幽灵节点请求：RequestProxy 的异步请求发送失败 → 立即回调错误 → 外层快速失败
	_, err := n1.Request(n1.CreateActorRef(clusterEchoType, "e"), newStrValMsg("q"), clusterTestTimeout)
	if err == nil || err.Code() != dvactor.ErrorCodeMessageSendFail {
		t.Fatalf("expected ErrorCodeMessageSendFail, got %v", err)
	}
}

// 回归测试（report 2026-08-30 附录 P0）：对端无法反序列化的消息会触发 server 关闭
// 会话；修复后本侧 OnDisconnect 正常触发、绑定被清理，client 按退避重连恢复。
func TestClusterUnknownMessageTypeDropsSessionAndReconnects(t *testing.T) {
	cl := dtu.StartCluster(t, []dtu.NodeSpec{
		{ // node1：server 侧，echo 只在此声明（放置确定），不注册 Int32Value
			ActorTypes: []vactor.ActorType{clusterEchoType},
			Register:   registerEcho,
		},
		{ // node2：client 侧，额外注册 Int32Value 作为 node1 无法反序列化的"毒消息"
			ActorTypes: nil,
			Register: func(s dvactor.ClusterSystem) {
				registerEcho(s)
				s.RegisterMessageType(clusterIntMsgType, intValCreator)
			},
		},
	})
	n2 := cl.Node(1)

	// 正常请求先确认链路可用
	rsp, err := n2.Request(n2.CreateActorRef(clusterEchoType, "e"), newStrValMsg("warm"), clusterTestTimeout)
	if err != nil {
		t.Fatalf("warmup request: %v", err)
	}
	_ = rsp

	// node2 发送 node1 无法反序列化的消息 → node1 关闭会话
	n2.Send(n2.CreateActorRef(clusterEchoType, "e"), newIntValMsg(1))
	vt.WaitFor(t, 5*time.Second, "session dropped on node1", func() bool {
		return cl.NodeLogsContain(0, "disconnected")
	})

	// client 约 5 秒后重连，链路恢复
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		rsp, err := n2.Request(n2.CreateActorRef(clusterEchoType, "e"), newStrValMsg("after"), 2*time.Second)
		if err == nil && rsp.(*wrapperspb.StringValue).GetValue() == "echo:after" {
			return // 重连成功
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("cluster did not recover after session drop")
}

// 跨节点超时：对端 actor 永不响应时，外层 Request 按调用方超时返回 ErrorCodeTimeout。
func TestClusterRequestTimeout(t *testing.T) {
	cl := dtu.StartCluster(t, []dtu.NodeSpec{
		{ActorTypes: []vactor.ActorType{clusterLocalType}, Register: registerMsgTypes},
		{ActorTypes: []vactor.ActorType{clusterEchoType}, Register: func(s dvactor.ClusterSystem) {
			registerMsgTypes(s)
			s.RegisterActorType(clusterEchoType, func() vactor.Actor {
				return func(ctx vactor.EnvelopeContext) {
					switch m := ctx.GetMessage().(type) {
					case *wrapperspb.StringValue:
						if m.GetValue() == "noresp" {
							return // 不响应
						}
						ctx.Response(newStrValMsg("echo"), nil)
					}
				}
			})
		}},
	})
	n1 := cl.Node(0)
	start := time.Now()
	_, err := n1.Request(n1.CreateActorRef(clusterEchoType, "e"), newStrValMsg("noresp"), 500*time.Millisecond)
	if err == nil || err.Code() != vactor.ErrorCodeTimeout {
		t.Fatalf("expected timeout, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout took too long: %v", elapsed)
	}
}

// 跨节点 BatchSend：两个目标都在远端节点，全部收到。
func TestClusterBatchSendCrossNode(t *testing.T) {
	col := &vt.Collector{}
	cl := dtu.StartCluster(t, []dtu.NodeSpec{
		{ActorTypes: []vactor.ActorType{clusterLocalType}, Register: registerMsgTypes},
		{ActorTypes: []vactor.ActorType{clusterTargetType}, Register: func(s dvactor.ClusterSystem) {
			registerMsgTypes(s)
			s.RegisterActorType(clusterTargetType, col.Creator())
		}},
	})
	n1 := cl.Node(0)
	refs := []vactor.ActorRef{
		n1.CreateActorRef(clusterTargetType, "t1"),
		n1.CreateActorRef(clusterTargetType, "t2"),
	}
	if err := n1.BatchSend(refs, []interface{}{newStrValMsg("m1"), newStrValMsg("m2")}); err != nil {
		t.Fatalf("batch send: %v", err)
	}
	// BatchSend 为笛卡尔广播：两个目标各收到全部 2 条
	msgs := col.WaitForMessages(t, 4, clusterTestTimeout, "batch cross-node")
	count := map[string]int{}
	for _, m := range msgs {
		count[m.(*wrapperspb.StringValue).GetValue()]++
	}
	if count["m1"] != 2 || count["m2"] != 2 {
		t.Fatalf("cross-product broken: %v", count)
	}
}

// 跨节点 actor 间同步请求：node1 上的 caller 用 ctx.Request 调 node2 上的 echo。
// 走的是 EnvelopeRequest / EnvelopeResponse 的跨节点编解码（此前无覆盖）。
func TestClusterInnerSyncRequestCrossNode(t *testing.T) {
	const clusterCallerType = dvactor.ActorTypeStart + 105
	result := make(chan string, 8)
	cl := dtu.StartCluster(t, []dtu.NodeSpec{
		{ActorTypes: []vactor.ActorType{clusterCallerType}, Register: func(s dvactor.ClusterSystem) {
			registerMsgTypes(s)
			s.RegisterActorType(clusterCallerType, func() vactor.Actor {
				return func(ctx vactor.EnvelopeContext) {
					if _, ok := ctx.GetMessage().(string); !ok {
						return
					}
					rsp, err := ctx.Request(ctx.CreateActorRef(clusterEchoType, "e1"), newStrValMsg("ping"), clusterTestTimeout)
					if err != nil {
						result <- "ERR:" + err.Error()
						return
					}
					if m, ok := rsp.(*wrapperspb.StringValue); ok {
						result <- m.GetValue()
						return
					}
					result <- "WRONG_TYPE"
				}
			})
		}},
		{ActorTypes: []vactor.ActorType{clusterEchoType}, Register: func(s dvactor.ClusterSystem) {
			registerMsgTypes(s)
			registerEcho(s)
		}},
	})
	cl.Node(0).Send(cl.Node(0).CreateActorRef(clusterCallerType, "c1"), "boot")
	if got := vt.WaitChan(t, result, clusterTestTimeout, "cross-node inner sync request"); got != "echo:ping" {
		t.Fatalf("got %q, want echo:ping", got)
	}
}

// 跨节点 actor 间同步请求的超时：远端永不响应时，请求方必须按时收到超时错误。
func TestClusterInnerSyncRequestTimeout(t *testing.T) {
	const clusterCallerType = dvactor.ActorTypeStart + 106
	result := make(chan vactor.VAError, 8)
	cl := dtu.StartCluster(t, []dtu.NodeSpec{
		{ActorTypes: []vactor.ActorType{clusterCallerType}, Register: func(s dvactor.ClusterSystem) {
			registerMsgTypes(s)
			s.RegisterActorType(clusterCallerType, func() vactor.Actor {
				return func(ctx vactor.EnvelopeContext) {
					if _, ok := ctx.GetMessage().(string); !ok {
						return
					}
					// "noresp" 让远端 echo 静默
					_, err := ctx.Request(ctx.CreateActorRef(clusterEchoType, "e1"), newStrValMsg("noresp"), 300*time.Millisecond)
					result <- err
				}
			})
		}},
		{ActorTypes: []vactor.ActorType{clusterEchoType}, Register: func(s dvactor.ClusterSystem) {
			registerMsgTypes(s)
			registerEcho(s)
		}},
	})
	cl.Node(0).Send(cl.Node(0).CreateActorRef(clusterCallerType, "c1"), "boot")
	err := vt.WaitChan(t, result, clusterTestTimeout, "cross-node inner sync timeout")
	if err == nil || err.Code() != vactor.ErrorCodeTimeout {
		t.Fatalf("expected timeout, got %v", err)
	}
}

// ListenHost 指定回环地址后集群仍能正常互连与通信（配置生效且不破坏组网）。
func TestClusterListenHostLoopback(t *testing.T) {
	cfgs := dtu.BuildClusterConfig(t, 2, [][]vactor.ActorType{{clusterEchoType}, {clusterEchoType}})
	for _, c := range cfgs {
		c.ListenHost = "127.0.0.1"
	}
	reg := func(s dvactor.ClusterSystem) {
		registerMsgTypes(s)
		registerEcho(s)
	}
	nodes := make([]dvactor.ClusterSystem, 2)
	for i := 0; i < 2; i++ {
		_, nodes[i] = dtu.NewNode(t, vactor.SystemId(i+1), cfgs, 5*time.Second, reg)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(s dvactor.ClusterSystem) {
			defer wg.Done()
			s.Start()
		}(nodes[i])
	}
	wg.Wait()
	for i, n := range nodes {
		if err := n.ClusterStartError(); err != nil {
			t.Fatalf("node %d failed to join with ListenHost set: %v", i+1, err)
		}
	}
	n1 := nodes[0]
	rsp, err := n1.Request(n1.CreateActorRef(clusterEchoType, "e"), newStrValMsg("ping"), clusterTestTimeout)
	if err != nil {
		t.Fatalf("cross-node request: %v", err)
	}
	if got := rsp.(*wrapperspb.StringValue).GetValue(); got != "echo:ping" {
		t.Fatalf("rsp = %q", got)
	}
}
