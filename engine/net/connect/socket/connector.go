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

func (cs *ConnectorSocket) Connect() error {
	conn, err := net.Dial("tcp", fmt.Sprintf("%v:%v", cs.host, cs.port))
	if err != nil {
		return err
	}

	tcpConn, ok := conn.(*net.TCPConn)
	if ok {
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}

	cs.conn = conn
	go cs.receiverRun()
	go cs.senderRun()
	go cs.heartbeatRun()
	if cs.onConnectFunc != nil {
		cs.onConnectFunc()
	}
	return nil
}

func (cs *ConnectorSocket) SetOnConnect(onConnectFunc netConnect.OnConnectFunc) {
	cs.onConnectFunc = onConnectFunc
}

func (cs *ConnectorSocket) SetAddress(host string, port uint16) {
	cs.host = host
	cs.port = port
}
