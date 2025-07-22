package socketNetConnect

import (
	"fmt"
	"net"
	"time"

	netConnect "github.com/kofplayer/dvactor/engine/net/connect"
)

func NewConnector() *ConnectorSocket {
	v := &ConnectorSocket{
		ConnSocket: newConn(nil),
	}
	return v
}

type ConnectorSocket struct {
	onConnectFunc netConnect.OnConnectFunc
	*ConnSocket
	host string
	port uint16
}

func (this *ConnectorSocket) Connect() error {
	conn, err := net.Dial("tcp", fmt.Sprintf("%v:%v", this.host, this.port))
	if err != nil {
		return err
	}

	tcpConn, ok := conn.(*net.TCPConn)
	if ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}

	this.ConnSocket.conn = conn
	go this.receiverRun()
	go this.senderRun()
	this.onConnectFunc()
	return nil
}

func (this *ConnectorSocket) SetOnConnect(onConnectFunc netConnect.OnConnectFunc) {
	this.onConnectFunc = onConnectFunc
}

func (this *ConnectorSocket) SetAddress(host string, port uint16) {
	this.host = host
	this.port = port
}
