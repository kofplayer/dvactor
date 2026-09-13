package socketNetConnect

import (
	"bufio"
	"errors"
	"net"
	"sync"
	"time"

	netConnect "github.com/kofplayer/dvactor/engine/net/connect"
	"github.com/kofplayer/vactor"
)

func newConn(conn net.Conn) *ConnSocket {
	// 心跳参数在连接建立时快照：之后只读实例字段。
	// 全局配置变量（netConnect.Heartbeat*）是测试与运行期可调的包级变量，
	// 若被运行中的连接持续读取，调整参数会构成数据竞争。
	return newConnWithHeartbeat(conn, netConnect.HeartbeatInterval, netConnect.HeartbeatTimeout)
}

// newConnWithHeartbeat 用显式心跳参数构造连接。
// 与 newConn 的区别是不读取全局配置——测试可据此注入参数，
// 避免在 accept 等并发路径上修改包级变量而形成数据竞争。
func newConnWithHeartbeat(conn net.Conn, heartbeatInterval, heartbeatTimeout time.Duration) *ConnSocket {
	v := new(ConnSocket)
	v.q = vactor.NewQueue[[]byte]()
	v.conn = conn
	v.heartbeatInterval = heartbeatInterval
	v.heartbeatTimeout = heartbeatTimeout
	return v
}

type ConnSocket struct {
	q                *vactor.Queue[[]byte]
	onDisconnectFunc netConnect.OnDisconnectFunc
	onDataFunc       netConnect.OnDataFunc
	conn             net.Conn
	// disconnectOnce 保证断线回调在多路径（读错误/写错误/本端主动关闭）下只触发一次
	disconnectOnce sync.Once

	// heartbeatInterval/heartbeatTimeout 为创建连接时的参数快照（创建后只读）
	heartbeatInterval time.Duration
	heartbeatTimeout  time.Duration
}

func (c *ConnSocket) RemoteAddr() string {
	if c.conn == nil || c.conn.RemoteAddr() == nil {
		return ""
	}
	return c.conn.RemoteAddr().String()
}

// Disconnect 通过关闭发送队列驱动 sender 退出再关闭底层连接；
// 断线回调由 sender/receiver 观察到关闭后经 notifyDisconnect 触发（恰好一次）。
func (c *ConnSocket) Disconnect() error {
	c.q.Close()
	return nil
}

// notifyDisconnect 触发断线回调（幂等）。无论断线由何种路径引起——读错误、
// 写错误或本端主动 Disconnect——都必须回调，否则本端绑定状态永不清理。
func (c *ConnSocket) notifyDisconnect() {
	c.disconnectOnce.Do(func() {
		if c.onDisconnectFunc != nil {
			c.onDisconnectFunc()
		}
	})
}

func (c *ConnSocket) SendData(data []byte) error {
	if !c.q.Enqueue(data) {
		return errors.New("connection closed")
	}
	return nil
}

func (c *ConnSocket) SetOnDisconnect(onDisconnectFunc netConnect.OnDisconnectFunc) {
	c.onDisconnectFunc = onDisconnectFunc
}

func (c *ConnSocket) SetOnData(onDataFunc netConnect.OnDataFunc) {
	c.onDataFunc = onDataFunc
}

func (c *ConnSocket) receiverRun() {
	reader := bufio.NewReader(c.conn)
	var buf [4096]byte
	for {
		if c.heartbeatTimeout > 0 {
			// 读超时兜底检测半开连接；任何收到的帧（含心跳 Pong）都会重置它
			_ = c.conn.SetReadDeadline(time.Now().Add(c.heartbeatTimeout))
		}
		n, err := reader.Read(buf[:])
		if err != nil {
			c.q.Close()
			c.notifyDisconnect()
			return
		}
		err = c.onDataFunc(buf[:n])
		if err != nil {
			c.q.Close()
			c.notifyDisconnect()
			return
		}
	}
}

func (c *ConnSocket) senderRun() {
	for {
		data, ok := c.q.Dequeue()
		if !ok {
			_ = c.conn.Close()
			return
		}
		msg := data
		for len(msg) > 0 {
			n, err := c.conn.Write(msg)
			if err != nil {
				// 写失败不能只静默退出：关闭连接并触发回调，
				// 否则发送队列无消费者、连接进入半死状态
				c.q.Close()
				_ = c.conn.Close()
				c.notifyDisconnect()
				return
			}
			msg = msg[n:]
		}
	}
}

// heartbeatRun 周期性向发送队列注入心跳帧，由接收方的读超时完成死链检测。
func (c *ConnSocket) heartbeatRun() {
	if c.heartbeatInterval <= 0 {
		return
	}
	ticker := time.NewTicker(c.heartbeatInterval)
	defer ticker.Stop()
	pkt, err := netConnect.PackMessage(netConnect.HeartbeatMsgIdPing, nil)
	if err != nil {
		return
	}
	for range ticker.C {
		if c.q.IsClosed() {
			return
		}
		_ = c.q.Enqueue(pkt) // 队列已关闭时返回 false，下轮退出
	}
}
