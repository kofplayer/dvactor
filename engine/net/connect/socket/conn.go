package socketNetConnect

import (
	"bufio"
	"net"

	netConnect "github.com/kofplayer/dvactor/engine/net/connect"

	"github.com/kofplayer/dvactor/engine/queue"
	queueDef "github.com/kofplayer/dvactor/engine/queue/def"
)

func newConn(conn net.Conn) *ConnSocket {
	v := new(ConnSocket)
	v.q = queue.NewQueue(32)
	v.conn = conn
	return v
}

type ConnSocket struct {
	q                queueDef.Queue
	onDisconnectFunc netConnect.OnDisconnectFunc
	onDataFunc       netConnect.OnDataFunc
	conn             net.Conn
}

func (this *ConnSocket) RemoteAddr() string {
	if this.conn == nil || this.conn.RemoteAddr() == nil {
		return ""
	}
	return this.conn.RemoteAddr().String()
}

func (this *ConnSocket) Disconnect() error {
	return this.q.Close()
}

func (this *ConnSocket) SendData(data []byte) error {
	err := this.q.Enqueue(data)
	if err != nil {
		return err
	}
	return nil
}

func (this *ConnSocket) SetOnDisconnect(onDisconnectFunc netConnect.OnDisconnectFunc) {
	this.onDisconnectFunc = onDisconnectFunc
}

func (this *ConnSocket) SetOnData(onDataFunc netConnect.OnDataFunc) {
	this.onDataFunc = onDataFunc
}

func (this *ConnSocket) receiverRun() {
	reader := bufio.NewReader(this.conn)
	var buf [4096]byte
	for {
		n, err := reader.Read(buf[:])
		if err != nil {
			if !this.q.IsClose() {
				this.Disconnect()
				this.onDisconnectFunc()
			}
			return
		}
		err = this.onDataFunc(buf[:n])
		if err != nil {
			if !this.q.IsClose() {
				this.Disconnect()
				this.onDisconnectFunc()
			}
			return
		}
	}
}

func (this *ConnSocket) senderRun() {
	for {
		data, ok := this.q.Dequeue()
		if !ok {
			this.conn.Close()
			return
		}
		msg := data.([]byte)
		for len(msg) > 0 {
			n, err := this.conn.Write(msg)
			if err != nil {
				return
			}
			msg = msg[n:]
		}
	}
}
