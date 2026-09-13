package socketNetConnect

import (
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

func (as *AcceptorSocket) Start() error {
	var err error
	as.listener, err = net.Listen("tcp", as.host+":"+strconv.Itoa(int(as.port)))
	if err != nil {
		as.notifyListen(err)
		return err
	}
	as.notifyListen(nil)
	for {
		conn, err := as.listener.Accept()
		if err != nil {
			return err
		}
		tcpConn, ok := conn.(*net.TCPConn)
		if ok {
			_ = tcpConn.SetKeepAlive(true)
			_ = tcpConn.SetKeepAlivePeriod(30 * time.Second)
		}
		c := newConn(conn)
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
