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

// Connect 建立到对端的 TCP 连接。
//
// 用 DialTimeout 而不是 net.Dial：黑洞地址（Host 写错、对端防火墙丢包）下
// net.Dial 会按内核 SYN 重试策略挂上数十秒到分钟级，期间调用方（集群重连循环）
// 既不能推进也不能被 Stop() 打断。
func (cs *ConnectorSocket) Connect() error {
	timeout := netConnect.DialTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second // 兜底：配置被改成非正值时仍要有上界
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%v:%v", cs.host, cs.port), timeout)
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
