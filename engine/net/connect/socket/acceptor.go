package socketNetConnect

import (
	"errors"
	"net"
	"strconv"
	"sync"
	"time"

	netConnect "github.com/kofplayer/dvactor/engine/net/connect"
)

func NewAcceptor() *AcceptorSocket {
	v := new(AcceptorSocket)
	return v
}

type AcceptorSocket struct {
	onAcceptFunc netConnect.OnAcceptFunc
	onListenFunc func(error)
	host         string
	port         uint16

	// mu 保护 listener：Start 在监听 goroutine 里写它，Stop 通常在另一个
	// goroutine 里读它并关闭。没有这把锁时二者构成数据竞争（-race 直接报
	// "Read at Stop / Previous write at Start"），而且 Stop 可能读到尚未赋值
	// 的 nil 而漏关监听器，留下一个再也没人能停的 accept 循环。
	mu       sync.Mutex
	listener net.Listener
}

// SetOnListen 设置监听结果回调：Start 后回调一次，err 为 nil 表示已成功监听
// 并进入 Accept 循环，否则为监听失败原因。用于上层同步感知端口绑定结果。
func (as *AcceptorSocket) SetOnListen(f func(error)) {
	as.onListenFunc = f
}

func (as *AcceptorSocket) notifyListen(err error) {
	if as.onListenFunc != nil {
		as.onListenFunc(err)
	}
}

// acceptRetryMin/Max 是 Accept 瞬时错误的退避区间。
const (
	acceptRetryMin = 5 * time.Millisecond
	acceptRetryMax = time.Second
)

// acceptError 判断 Accept 错误是否属于"稍后再试"的瞬时错误。
//
// 为什么不能一有错误就 return：fd 耗尽（EMFILE）、监听 backlog 满导致的
// ECONNABORTED、偶发的 EINTR 都是可恢复的，退出循环会让本节点**静默失去入站
// 能力**且永不恢复（上层只来得及记一行日志）。识别不了的错误一律按瞬时处理并
// 退避重试，只有 listener 自身被关闭才退出——那才是真正该结束的信号。
func acceptErrorIsTransient(err error) bool {
	if errors.Is(err, net.ErrClosed) {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	type temporary interface{ Temporary() bool }
	var te temporary
	if errors.As(err, &te) {
		return te.Temporary()
	}
	// 未知形态：继续循环（带退避）比永久停摆更安全
	return true
}

func (as *AcceptorSocket) Start() error {
	ln, err := net.Listen("tcp", as.host+":"+strconv.Itoa(int(as.port)))
	if err != nil {
		as.notifyListen(err)
		return err
	}
	as.mu.Lock()
	as.listener = ln
	as.mu.Unlock()
	as.notifyListen(nil)
	backoff := acceptRetryMin
	for {
		conn, err := ln.Accept()
		if err != nil {
			if !acceptErrorIsTransient(err) {
				return err
			}
			time.Sleep(backoff)
			backoff *= 2
			if backoff > acceptRetryMax {
				backoff = acceptRetryMax
			}
			continue
		}
		backoff = acceptRetryMin

		tcpConn, ok := conn.(*net.TCPConn)
		if ok {
			_ = tcpConn.SetKeepAlive(true)
			_ = tcpConn.SetKeepAlivePeriod(30 * time.Second)
		}
		c := newConn(conn)
		// 回调刻意同步执行：它只做"登记会话 + 设置回调"，收发循环在其后才启动，
		// 因此回调里 SetOnData/SetOnDisconnect 与 receiverRun 之间天然有序。
		// 上层若要在其中做耗时工作，应自行异步化——同步跑是这条时序的代价与前提。
		if as.onAcceptFunc == nil {
			// 未设置回调也要把 fd 收掉，否则每来一个连接就泄漏一个 socket
			_ = conn.Close()
			continue
		}
		as.onAcceptFunc(c)
		go c.receiverRun()
		go c.senderRun()
		go c.heartbeatRun()
	}
}

func (as *AcceptorSocket) Stop() error {
	as.mu.Lock()
	ln := as.listener
	as.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	return nil
}

// addr 返回已绑定监听器的地址；未开始监听时返回 nil。
// 测试要连"Start 之后真正的端口"（SetAddress 传 0 时端口由内核分配），
// 直接读 listener 字段会与 Start 的写入构成数据竞争。
func (as *AcceptorSocket) addr() net.Addr {
	as.mu.Lock()
	defer as.mu.Unlock()
	if as.listener == nil {
		return nil
	}
	return as.listener.Addr()
}

func (as *AcceptorSocket) SetOnAccept(onAcceptFunc netConnect.OnAcceptFunc) {
	as.onAcceptFunc = onAcceptFunc
}

func (as *AcceptorSocket) SetAddress(host string, port uint16) {
	as.host = host
	as.port = port
}
