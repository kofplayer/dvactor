package server_test

import (
	"time"

	"testing"

	netclient "github.com/kofplayer/dvactor/engine/net/client"
	netConnect "github.com/kofplayer/dvactor/engine/net/connect"
	socketNetConnect "github.com/kofplayer/dvactor/engine/net/connect/socket"
	netserver "github.com/kofplayer/dvactor/engine/net/server"
	netSession "github.com/kofplayer/dvactor/engine/net/session"
	vt "github.com/kofplayer/vactor/testutil"
)

type frame struct {
	msgId uint32
	data  string
}

// 启动真实 TCP server（监听临时端口）+ client，并等待连接建立。
// 返回 server/client 的收帧通道与断线信号。
func startServerClient(t *testing.T) (chan frame, chan frame, chan struct{}, netclient.NetClient) {
	t.Helper()
	ports := vt.FreePorts(t, 1)
	port := uint16(ports[0])

	srvFrames := make(chan frame, 16)
	cliFrames := make(chan frame, 16)
	srvDisconnected := make(chan struct{}, 4)

	acceptor := socketNetConnect.NewAcceptor()
	acceptor.SetAddress("", port)
	srv := netserver.NewNetServer()
	srv.SetAcceptor(acceptor)
	srv.SetOnConnect(func(s netSession.NetSession) {})
	srv.SetOnMessage(func(s netSession.NetSession, msgId uint32, data []byte) error {
		srvFrames <- frame{msgId, string(data)}
		_ = s.SendMessage(msgId+100, []byte("re:"+string(data)))
		return nil
	})
	srv.SetOnDisconnect(func(s netSession.NetSession) {
		srvDisconnected <- struct{}{}
	})
	started := make(chan error, 1)
	go func() {
		// Accept 循环阻塞；端口已预分配，失败会表现为连接失败
		started <- srv.Start()
	}()
	// 必须在测试结束前停掉监听循环：残留的 accept goroutine 会继续读包级配置
	// （newConn 每次 accept 都读 HeartbeatInterval/WriteTimeout），与后续测试写这些
	// 变量构成数据竞争。-race 下表现为 "Write at ... by TestX / Previous read at
	// ... by acceptor.Start" 的跨测试报告。
	t.Cleanup(func() {
		_ = srv.Stop()
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Error("accept loop did not exit after Stop")
		}
	})

	cli := netclient.NewNetClient()
	conn := socketNetConnect.NewConnector()
	conn.SetAddress("127.0.0.1", port)
	cli.SetConnector(conn)
	cli.SetOnMessage(func(msgId uint32, data []byte) error {
		cliFrames <- frame{msgId, string(data)}
		return nil
	})

	// 重试连接直至 acceptor 就绪
	connected := false
	for i := 0; i < 100 && !connected; i++ {
		if err := cli.Connect(); err == nil {
			connected = true
		} else {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !connected {
		t.Fatal("client failed to connect")
	}
	return srvFrames, cliFrames, srvDisconnected, cli
}

func TestSocketRoundtrip(t *testing.T) {
	srvFrames, cliFrames, _, cli := startServerClient(t)

	if err := cli.SendMessage(7, []byte("ping")); err != nil {
		t.Fatalf("send: %v", err)
	}
	f := vt.WaitChan(t, srvFrames, 3*time.Second, "server received frame")
	if f.msgId != 7 || f.data != "ping" {
		t.Fatalf("server frame = %+v", f)
	}
	r := vt.WaitChan(t, cliFrames, 3*time.Second, "client received reply")
	if r.msgId != 107 || r.data != "re:ping" {
		t.Fatalf("client frame = %+v", r)
	}
}

// 客户端主动断开：server 侧 OnDisconnect 必须触发（会话清理依赖它）。
func TestSocketClientDisconnectFiresServerCallback(t *testing.T) {
	srvFrames, _, srvDisconnected, cli := startServerClient(t)
	_ = cli.SendMessage(1, []byte("x"))
	vt.WaitChan(t, srvFrames, 3*time.Second, "frame before disconnect")

	if err := cli.Disconnect(); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	vt.WaitFor(t, 3*time.Second, "server saw disconnect", func() bool {
		select {
		case <-srvDisconnected:
			return true
		default:
			return false
		}
	})
}

// handler 出错时由上层在 wrapper 中主动 Close 会话（clusterServer 的做法，
// netServer 自身会忽略 handler 错误），client 侧应感知断线。
func TestSocketHandlerErrorClosesSession(t *testing.T) {
	ports := vt.FreePorts(t, 1)
	port := uint16(ports[0])

	acceptor := socketNetConnect.NewAcceptor()
	acceptor.SetAddress("", port)
	srv := netserver.NewNetServer()
	srv.SetAcceptor(acceptor)
	srv.SetOnConnect(func(s netSession.NetSession) {})
	srv.SetOnMessage(func(s netSession.NetSession, msgId uint32, data []byte) error {
		if msgId == 9 {
			err := &testErr{}
			_ = s.Close() // 上层主动关闭会话（netServer 不处理 handler 错误）
			return err
		}
		return nil
	})
	started := make(chan error, 1)
	go func() { started <- srv.Start() }()
	// 同 startServerClient：监听循环必须随测试一起收掉，否则它会跨测试存活并
	// 继续读包级配置，和后面的心跳测试写配置构成数据竞争。
	t.Cleanup(func() {
		_ = srv.Stop()
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Error("accept loop did not exit after Stop")
		}
	})

	cliDisconnected := make(chan struct{}, 4)
	cli := netclient.NewNetClient()
	conn := socketNetConnect.NewConnector()
	conn.SetAddress("127.0.0.1", port)
	cli.SetConnector(conn)
	cli.SetOnDisconnect(func() { cliDisconnected <- struct{}{} })
	connected := false
	for i := 0; i < 100 && !connected; i++ {
		if err := cli.Connect(); err == nil {
			connected = true
		} else {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !connected {
		t.Fatal("connect failed")
	}

	_ = cli.SendMessage(9, []byte("poison"))
	vt.WaitFor(t, 3*time.Second, "client saw disconnect after handler error", func() bool {
		select {
		case <-cliDisconnected:
			return true
		default:
			return false
		}
	})
}

type testErr struct{}

func (*testErr) Error() string { return "handler error" }

// 心跳保活：零业务流量下，连接依靠 Ping/Pong 保持存活越过读超时窗口。
func TestSocketHeartbeatKeepsIdleConnectionAlive(t *testing.T) {
	// 缩短心跳参数（测试进程内独占，测试前设置、结束后恢复）
	origInterval, origTimeout := netConnect.HeartbeatInterval, netConnect.HeartbeatTimeout
	netConnect.HeartbeatInterval, netConnect.HeartbeatTimeout = 100*time.Millisecond, 400*time.Millisecond
	defer func() {
		netConnect.HeartbeatInterval, netConnect.HeartbeatTimeout = origInterval, origTimeout
	}()

	srvFrames, cliFrames, srvDisconnected, cli := startServerClient(t)
	_ = srvFrames
	_ = cliFrames

	// 1.2 秒无任何业务流量（> 3 倍读超时窗口的一半，足以覆盖多个心跳周期），心跳必须维持连接
	time.Sleep(1200 * time.Millisecond)
	select {
	case <-srvDisconnected:
		t.Fatal("idle connection was dropped, heartbeat failed")
	default:
	}
	if err := cli.SendMessage(5, []byte("still-alive")); err != nil {
		t.Fatalf("send after idle: %v", err)
	}
	f := vt.WaitChan(t, srvFrames, 3*time.Second, "frame after idle period")
	if f.data != "still-alive" {
		t.Fatalf("frame = %+v", f)
	}
}

// 大量帧往返：验证底层拆包/发送队列的吞吐与完整性。
func TestSocketManyFrames(t *testing.T) {
	srvFrames, cliFrames, _, cli := startServerClient(t)
	const n = 500
	for i := 0; i < n; i++ {
		if err := cli.SendMessage(uint32(i%200+1), []byte{byte(i % 256)}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	got := 0
	deadline := time.Now().Add(5 * time.Second)
	for got < n && time.Now().Before(deadline) {
		select {
		case <-srvFrames:
			got++
		case f := <-cliFrames:
			if len(f.data) < 3 || f.data[:3] != "re:" {
				t.Fatalf("unexpected reply %+v", f)
			}
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	if got != n {
		t.Fatalf("server received %d/%d frames", got, n)
	}
	_ = netConnect.PacketHeaderSize // 保持 netConnect 导入（帧头大小常量）
}
