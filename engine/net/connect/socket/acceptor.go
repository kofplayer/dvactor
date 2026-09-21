package socketNetConnect

import (
	"errors"
	"net"
	"strconv"
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
	listener     net.Listener
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
	var err error
	as.listener, err = net.Listen("tcp", as.host+":"+strconv.Itoa(int(as.port)))
	if err != nil {
		as.notifyListen(err)
		return err
	}
	as.notifyListen(nil)
	backoff := acceptRetryMin
	for {
		conn, err := as.listener.Accept()
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
	if as.listener != nil {
		_ = as.listener.Close()
	}
	return nil
}

func (as *AcceptorSocket) SetOnAccept(onAcceptFunc netConnect.OnAcceptFunc) {
	as.onAcceptFunc = onAcceptFunc
}

func (as *AcceptorSocket) SetAddress(host string, port uint16) {
	as.host = host
	as.port = port
}
