package server_test

import (
	"sync"
	"testing"

	netConnect "github.com/kofplayer/dvactor/engine/net/connect"
	netserver "github.com/kofplayer/dvactor/engine/net/server"
)

// stubAcceptor 把 onAccept 暴露给测试，无需真起监听器。
type stubAcceptor struct {
	mu    sync.Mutex
	onAcc netConnect.OnAcceptFunc
}

func (a *stubAcceptor) SetOnAccept(f netConnect.OnAcceptFunc) {
	a.mu.Lock()
	a.onAcc = f
	a.mu.Unlock()
}

func (a *stubAcceptor) Start() error { return nil }
func (a *stubAcceptor) Stop() error  { return nil }

func (a *stubAcceptor) accept(c netConnect.Conn) {
	a.mu.Lock()
	f := a.onAcc
	a.mu.Unlock()
	if f != nil {
		f(c)
	}
}

// stubConn 只记录是否被拒（Disconnect 被调用）。
type stubConn struct {
	mu      sync.Mutex
	closed  bool
	sendErr error
	onData  netConnect.OnDataFunc
	onDisc  netConnect.OnDisconnectFunc
	remote  string
}

func (c *stubConn) RemoteAddr() string { return c.remote }
func (c *stubConn) Disconnect() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}
func (c *stubConn) SendData([]byte) error { return c.sendErr }
func (c *stubConn) SetOnDisconnect(f netConnect.OnDisconnectFunc) {
	c.mu.Lock()
	c.onDisc = f
	c.mu.Unlock()
}
func (c *stubConn) SetOnData(f netConnect.OnDataFunc) {
	c.mu.Lock()
	c.onData = f
	c.mu.Unlock()
}

func (c *stubConn) wasClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// accept 即登记会话，未注册的连接要等读超时（默认 60s）才回收。没有上限时，
// 端口扫描/握手洪泛可以让 session 表与 fd 无界膨胀，所以必须能拒绝超额连接：
// 超额连接不进表、被关掉，并且给出可排查的回调。
func TestServerRejectsSessionsAboveCap(t *testing.T) {
	acc := &stubAcceptor{}
	svr := netserver.NewNetServer()
	svr.SetAcceptor(acc)
	svr.SetMaxSessions(2)

	var mu sync.Mutex
	rejected := []string{}
	svr.SetOnReject(func(remoteAddr string, limit int) {
		mu.Lock()
		rejected = append(rejected, remoteAddr)
		mu.Unlock()
	})
	if err := svr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	conns := make([]*stubConn, 0, 5)
	for i := 0; i < 5; i++ {
		c := &stubConn{remote: "peer"}
		conns = append(conns, c)
		acc.accept(c)
	}

	if got := svr.GetSessionMgr().Count(); got != 2 {
		t.Fatalf("registered sessions = %d, want exactly the cap (2)", got)
	}
	mu.Lock()
	gotRejected := len(rejected)
	mu.Unlock()
	if gotRejected != 3 {
		t.Fatalf("reject callbacks = %d, want 3", gotRejected)
	}
	for i, c := range conns {
		switch {
		case i < 2 && c.wasClosed():
			t.Fatalf("session %d was within the cap but got disconnected", i)
		case i >= 2 && !c.wasClosed():
			t.Fatalf("excess connection %d was not closed", i)
		}
	}
}

// 不设上限时保持旧行为：全部登记。
func TestServerUnlimitedByDefault(t *testing.T) {
	acc := &stubAcceptor{}
	svr := netserver.NewNetServer()
	svr.SetAcceptor(acc)
	if err := svr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for i := 0; i < 6; i++ {
		acc.accept(&stubConn{remote: "peer"})
	}
	if got := svr.GetSessionMgr().Count(); got != 6 {
		t.Fatalf("registered sessions = %d, want 6 (no cap configured)", got)
	}
}
