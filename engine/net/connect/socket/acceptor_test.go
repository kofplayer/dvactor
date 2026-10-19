package socketNetConnect

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"

	netConnect "github.com/kofplayer/dvactor/engine/net/connect"
)

// fakeTimeoutError 实现 net.Error 且 Timeout()==true，用于验证"偶发超时不算停摆"。
type fakeTimeoutError struct{}

func (fakeTimeoutError) Error() string   { return "i/o timeout" }
func (fakeTimeoutError) Timeout() bool   { return true }
func (fakeTimeoutError) Temporary() bool { return true }

// Accept 的瞬时错误必须重试而不是让监听器永久停摆：fd 耗尽（EMFILE）、backlog 满
// 导致的 ECONNABORTED 都是可恢复的，直接 return 会让本节点静默失去入站能力。
//
// 只断言与平台无关的三类判定：syscall.Errno.Temporary() 在不同 OS 上取值不同
// （Windows 上基本恒为 false），所以 errno 的具体分类交由下面的"真实关闭"用例
// 与 OS 语义决定；关键是**认不出形态的错误必须留在重试侧**，不能退出循环。
func TestAcceptErrorTransientClassification(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		transient bool
	}{
		{"listener closed", net.ErrClosed, false},
		{"wrapped listener closed", errors.Join(errors.New("accept"), net.ErrClosed), false},
		{"net.Error timeout", fakeTimeoutError{}, true},
		{"os deadline exceeded", os.ErrDeadlineExceeded, true},
		{"unknown shape stays retryable", errors.New("some random error"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := acceptErrorIsTransient(c.err); got != c.transient {
				t.Fatalf("acceptErrorIsTransient(%v) = %v, want %v", c.err, got, c.transient)
			}
		})
	}
}

// 端到端：listener 被关闭后 Accept 的错误必须走"退出"分支（停机依赖它返回）。
func TestAcceptLoopExitsWhenListenerClosed(t *testing.T) {
	acc := NewAcceptor()
	acc.SetAddress("127.0.0.1", 0)
	acc.SetOnAccept(func(netConnect.Conn) { t.Error("no connection expected") })
	listenErr := make(chan error, 1)
	acc.SetOnListen(func(err error) { listenErr <- err })
	done := make(chan error, 1)
	go func() { done <- acc.Start() }()
	select {
	case err := <-listenErr:
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no listen callback")
	}
	if err := acc.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accept loop should report the closing error")
		}
		if acceptErrorIsTransient(err) {
			t.Fatalf("accept loop exited with a retryable error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("accept loop did not exit after the listener was closed")
	}
}

// 未设置 onAcceptFunc 时不能 panic，也不能泄漏 fd。
func TestAcceptWithoutCallbackClosesConnection(t *testing.T) {
	acc := NewAcceptor()
	acc.SetAddress("127.0.0.1", 0)
	listenErr := make(chan error, 1)
	acc.SetOnListen(func(err error) { listenErr <- err })
	go func() { _ = acc.Start() }()
	select {
	case err := <-listenErr:
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no listen callback")
	}
	defer func() { _ = acc.Stop() }()

	conn, err := net.Dial("tcp", acc.addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	// 没有回调：本端应直接关掉这条连接，读它最终会得到错误而不是永久挂起
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection without an accept callback should be closed by the acceptor")
	}
}
