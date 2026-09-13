package socketNetConnect

import (
	"net"
	"sync"
	"testing"
	"time"

	netConnect "github.com/kofplayer/dvactor/engine/net/connect"
)

// 说明：本文件不修改包级心跳配置（netConnect.Heartbeat*）——acceptor 的 accept
// 循环在独立 goroutine 中读取该配置，测试若并发写会构成数据竞争。
// 需要精确控制心跳的用例统一走 newConnWithHeartbeat 注入参数。

// serverCapture 采集 accept 侧连接与其收到的数据。
type serverCapture struct {
	mu     sync.Mutex
	conn   netConnect.Conn
	data   [][]byte
	closed chan struct{}
	once   sync.Once
}

func (s *serverCapture) push(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = append(s.data, append([]byte(nil), b...))
}

func (s *serverCapture) snapshot() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(s.data))
	copy(out, s.data)
	return out
}

// startAcceptor 启动一个本地 acceptor，返回其端口与 accept 采集器。
func startAcceptor(t *testing.T) (*AcceptorSocket, int, *serverCapture) {
	t.Helper()
	acc := NewAcceptor()
	acc.SetAddress("127.0.0.1", 0) // 端口 0：由内核分配
	cap := &serverCapture{closed: make(chan struct{})}
	acc.SetOnAccept(func(c netConnect.Conn) {
		cap.mu.Lock()
		cap.conn = c
		cap.mu.Unlock()
		c.SetOnData(func(b []byte) error {
			cap.push(b)
			return nil
		})
		c.SetOnDisconnect(func() {
			cap.once.Do(func() { close(cap.closed) })
		})
	})
	listenErr := make(chan error, 1)
	acc.SetOnListen(func(err error) { listenErr <- err })
	go func() { _ = acc.Start() }()
	select {
	case err := <-listenErr:
		if err != nil {
			t.Fatalf("listen failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("acceptor did not report listen result")
	}
	addr := acc.listener.Addr().(*net.TCPAddr)
	t.Cleanup(func() { _ = acc.Stop() })
	return acc, addr.Port, cap
}

// newPair 用 net.Pipe 造一对已连通的 socket，并注入指定的心跳参数。
// setup 在读/写循环启动前调用，用于注册回调（避免回调字段与 goroutine 竞争）。
func newPair(t *testing.T, interval, timeout time.Duration, setup func(cli, srv *ConnSocket)) (*ConnSocket, *ConnSocket) {
	t.Helper()
	c1, c2 := net.Pipe()
	cli := newConnWithHeartbeat(c1, interval, timeout)
	srv := newConnWithHeartbeat(c2, interval, timeout)
	if setup != nil {
		setup(cli, srv)
	}
	go cli.receiverRun()
	go cli.senderRun()
	go cli.heartbeatRun()
	go srv.receiverRun()
	go srv.senderRun()
	go srv.heartbeatRun()
	t.Cleanup(func() {
		_ = cli.Disconnect()
		_ = srv.Disconnect()
	})
	return cli, srv
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// 真实 TCP 端到端：连接、双向收发、RemoteAddr、断开回调、关闭后发送报错。
func TestSocketEndToEnd(t *testing.T) {
	_, port, cap := startAcceptor(t)

	cli := NewConnector()
	cli.SetAddress("127.0.0.1", uint16(port))
	connected := make(chan struct{}, 1)
	cli.SetOnConnect(func() { connected <- struct{}{} })
	var mu sync.Mutex
	var got [][]byte
	cli.SetOnData(func(b []byte) error {
		mu.Lock()
		got = append(got, append([]byte(nil), b...))
		mu.Unlock()
		return nil
	})
	disconnected := make(chan struct{}, 1)
	cli.SetOnDisconnect(func() { disconnected <- struct{}{} })

	if err := cli.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("onConnect not fired")
	}
	if cli.RemoteAddr() == "" {
		t.Fatal("RemoteAddr should not be empty on a live connection")
	}

	// client -> server
	if err := cli.SendData([]byte("ping-from-client")); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitFor(t, 3*time.Second, "server receives payload", func() bool {
		return len(cap.snapshot()) > 0
	})
	if snaps := cap.snapshot(); string(snaps[0]) != "ping-from-client" {
		t.Fatalf("server payload = %q", snaps[0])
	}

	// server -> client
	cap.mu.Lock()
	srvConn := cap.conn
	cap.mu.Unlock()
	if err := srvConn.SendData([]byte("pong-from-server")); err != nil {
		t.Fatalf("server send: %v", err)
	}
	waitFor(t, 3*time.Second, "client receives payload", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) > 0
	})
	mu.Lock()
	if string(got[0]) != "pong-from-server" {
		mu.Unlock()
		t.Fatalf("client payload = %q", got[0])
	}
	mu.Unlock()

	// 主动断开：两端断线回调各触发一次
	if err := cli.Disconnect(); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("client onDisconnect not fired after Disconnect()")
	}
	select {
	case <-cap.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("server onDisconnect not fired after peer close")
	}
	if err := cli.SendData([]byte("after-close")); err == nil {
		t.Fatal("SendData on a closed connection should fail")
	}
}

// 断线回调幂等：多次触发只回调一次。
func TestSocketDisconnectCallbackIsOnce(t *testing.T) {
	_, port, _ := startAcceptor(t)

	cli := NewConnector()
	cli.SetAddress("127.0.0.1", uint16(port))
	cli.SetOnConnect(func() {})
	cli.SetOnData(func([]byte) error { return nil })
	var count int
	var mu sync.Mutex
	done := make(chan struct{}, 4)
	cli.SetOnDisconnect(func() {
		mu.Lock()
		count++
		mu.Unlock()
		done <- struct{}{}
	})
	if err := cli.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	_ = cli.Disconnect()
	_ = cli.Disconnect()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("onDisconnect never fired")
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Fatalf("onDisconnect fired %d times, want 1", count)
	}
}

// 注入式连接的收发与 RemoteAddr（net.Pipe 对）。
func TestSocketPairSendReceiveAndRemoteAddr(t *testing.T) {
	var mu sync.Mutex
	var srvGot, cliGot [][]byte
	cli, srv := newPair(t, 0, 0, func(cli, srv *ConnSocket) {
		cli.SetOnData(func(b []byte) error {
			mu.Lock()
			cliGot = append(cliGot, append([]byte(nil), b...))
			mu.Unlock()
			return nil
		})
		srv.SetOnData(func(b []byte) error {
			mu.Lock()
			srvGot = append(srvGot, append([]byte(nil), b...))
			mu.Unlock()
			return nil
		})
		cli.SetOnDisconnect(func() {})
		srv.SetOnDisconnect(func() {})
	})
	if cli.RemoteAddr() == "" || srv.RemoteAddr() == "" {
		t.Fatal("RemoteAddr should be non-empty for a piped connection")
	}
	if err := cli.SendData([]byte("to-server")); err != nil {
		t.Fatalf("cli send: %v", err)
	}
	waitFor(t, 2*time.Second, "server receives", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(srvGot) > 0
	})
	if err := srv.SendData([]byte("to-client")); err != nil {
		t.Fatalf("srv send: %v", err)
	}
	waitFor(t, 2*time.Second, "client receives", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(cliGot) > 0
	})
	mu.Lock()
	defer mu.Unlock()
	if string(srvGot[0]) != "to-server" || string(cliGot[0]) != "to-client" {
		t.Fatalf("payload mismatch: %q / %q", srvGot[0], cliGot[0])
	}
}

// 心跳让空闲连接保持存活：Ping/Pong 交换使读超时不会误判死链。
func TestSocketHeartbeatKeepsConnectionAlive(t *testing.T) {
	died := make(chan string, 2)
	_, _ = newPair(t, 20*time.Millisecond, 400*time.Millisecond, func(cli, srv *ConnSocket) {
		cli.SetOnData(func([]byte) error { return nil })
		srv.SetOnData(func([]byte) error { return nil })
		cli.SetOnDisconnect(func() { died <- "cli" })
		srv.SetOnDisconnect(func() { died <- "srv" })
	})
	select {
	case who := <-died:
		t.Fatalf("%s dropped an idle connection despite heartbeats", who)
	case <-time.After(700 * time.Millisecond):
	}
}

// 读超时能检测半开连接：心跳关闭且对端静默时连接被判死。
func TestSocketReadTimeoutDetectsDeadPeer(t *testing.T) {
	died := make(chan string, 2)
	_, _ = newPair(t, 0, 150*time.Millisecond, func(cli, srv *ConnSocket) {
		cli.SetOnData(func([]byte) error { return nil })
		srv.SetOnData(func([]byte) error { return nil })
		cli.SetOnDisconnect(func() { died <- "cli" })
		srv.SetOnDisconnect(func() { died <- "srv" })
	})
	select {
	case <-died:
	case <-time.After(3 * time.Second):
		t.Fatal("read deadline did not detect the silent peer")
	}
}

// 端口占用：Start 必须同步回报监听失败，而不是静默重试。
func TestAcceptorReportsListenFailure(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port

	acc := NewAcceptor()
	acc.SetAddress("127.0.0.1", uint16(port))
	acc.SetOnAccept(func(netConnect.Conn) {})
	result := make(chan error, 1)
	acc.SetOnListen(func(err error) { result <- err })
	go func() { _ = acc.Start() }()
	select {
	case got := <-result:
		if got == nil {
			t.Fatal("expected listen error for an occupied port")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no listen result reported")
	}
}

// 连接不可达：Connect 返回错误且不触发 onConnect。
func TestConnectorDialFailure(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	cli := NewConnector()
	cli.SetAddress("127.0.0.1", uint16(port))
	fired := false
	cli.SetOnConnect(func() { fired = true })
	if err := cli.Connect(); err == nil {
		t.Fatal("dial to a closed port should fail")
	}
	if fired {
		t.Fatal("onConnect must not fire when dial fails")
	}
}

// 未设置 onConnect 回调时 Connect 不得 panic（与 acceptor 的 nil 检查对称）。
func TestConnectorConnectWithoutCallback(t *testing.T) {
	_, port, _ := startAcceptor(t)

	cli := NewConnector()
	cli.SetAddress("127.0.0.1", uint16(port))
	if err := cli.Connect(); err != nil {
		t.Fatalf("connect without onConnect callback: %v", err)
	}
	_ = cli.Disconnect()
}

// 连接已被断开后发送数据必须返回错误（发送队列已关闭）。
func TestSocketSendAfterPeerClose(t *testing.T) {
	cli, srv := newPair(t, 0, 0, func(cli, srv *ConnSocket) {
		cli.SetOnData(func([]byte) error { return nil })
		srv.SetOnData(func([]byte) error { return nil })
		cli.SetOnDisconnect(func() {})
		srv.SetOnDisconnect(func() {})
	})
	_ = srv.Disconnect()
	waitFor(t, 2*time.Second, "cli observes peer close", func() bool {
		return cli.q.IsClosed()
	})
	if err := cli.SendData([]byte("x")); err == nil {
		t.Fatal("SendData should fail once the send queue is closed")
	}
}
