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
func (this *AcceptorSocket) SetOnListen(f func(error)) {
	this.onListenFunc = f
}

func (this *AcceptorSocket) notifyListen(err error) {
	if this.onListenFunc != nil {
		this.onListenFunc(err)
	}
}

func (this *AcceptorSocket) Start() error {
	var err error
	this.listener, err = net.Listen("tcp", this.host+":"+strconv.Itoa(int(this.port)))
	if err != nil {
		this.notifyListen(err)
		return err
	}
	this.notifyListen(nil)
	for {
		conn, err := this.listener.Accept()
		if err != nil {
			return err
		}
		tcpConn, ok := conn.(*net.TCPConn)
		if ok {
			tcpConn.SetKeepAlive(true)
			tcpConn.SetKeepAlivePeriod(30 * time.Second)
		}
		c := newConn(conn)
		this.onAcceptFunc(c)
		go c.receiverRun()
		go c.senderRun()
		go c.heartbeatRun()
	}
}

func (this *AcceptorSocket) Stop() error {
	if this.listener != nil {
		this.listener.Close()
	}
	return nil
}

func (this *AcceptorSocket) SetOnAccept(onAcceptFunc netConnect.OnAcceptFunc) {
	this.onAcceptFunc = onAcceptFunc
}

func (this *AcceptorSocket) SetAddress(host string, port uint16) {
	this.host = host
	this.port = port
}
