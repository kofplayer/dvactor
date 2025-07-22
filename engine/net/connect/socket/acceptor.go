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
	host         string
	port         uint16
	listener     net.Listener
}

func (this *AcceptorSocket) Start() error {
	var err error
	this.listener, err = net.Listen("tcp", this.host+":"+strconv.Itoa(int(this.port)))
	if err != nil {
		return err
	}
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
