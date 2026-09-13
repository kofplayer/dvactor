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
		s.SendMessage(msgId+100, []byte("re:"+string(data)))
		return nil
	})
	srv.SetOnDisconnect(func(s netSession.NetSession) {
		srvDisconnected <- struct{}{}
	})
	go func() {
		_ = srv.Start() // Accept 循环阻塞；端口已预分配，失败会表现为连接失败
	}()

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
	cli.SendMessage(1, []byte("x"))
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
			s.Close() // 上层主动关闭会话（netServer 不处理 handler 错误）
			return err
		}
		return nil
	})
	go func() { _ = srv.Start() }()

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

	cli.SendMessage(9, []byte("poison"))
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
