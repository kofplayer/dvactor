package socketNetConnect

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	netConnect "github.com/kofplayer/dvactor/engine/net/connect"
	"github.com/kofplayer/vactor"
)

// DisconnectGrace 主动断开时留给已入队数据写出的窗口；到点无条件关闭 fd。
const DisconnectGrace = 100 * time.Millisecond

func newConn(conn net.Conn) *ConnSocket {
	// 心跳与写超时在连接建立时快照：之后只读实例字段。
	// 全局配置变量（netConnect.*）是测试与运行期可调的包级变量，
	// 若被运行中的连接持续读取，调整参数会构成数据竞争。
	return newConnWithHeartbeat(conn, netConnect.HeartbeatInterval, netConnect.HeartbeatTimeout)
}

// newConnWithHeartbeat 用显式心跳参数构造连接。
// 与 newConn 的区别是不读取心跳配置——测试可据此注入参数，
// 避免在 accept 等并发路径上修改包级变量而形成数据竞争。
func newConnWithHeartbeat(conn net.Conn, heartbeatInterval, heartbeatTimeout time.Duration) *ConnSocket {
	v := new(ConnSocket)
	v.q = vactor.NewQueue[[]byte]()
	// 有界发送队列：对端不收时无界队列会把本端内存吃到 OOM，而 SendData 一路返回
	// "成功"、上层据此以为消息已送达。队满必须变成一次可见的错误（上层据此降级）。
	v.q.SetMaxDepth(netConnect.SendQueueMaxFrames)
	v.conn = conn
	v.heartbeatInterval = heartbeatInterval
	v.heartbeatTimeout = heartbeatTimeout
	v.writeTimeout = netConnect.WriteTimeout
	return v
}

type ConnSocket struct {
	q                *vactor.Queue[[]byte]
	onDisconnectFunc netConnect.OnDisconnectFunc
	onDataFunc       netConnect.OnDataFunc
	conn             net.Conn
	// disconnectOnce 保证断线回调在多路径（读错误/写错误/本端主动关闭）下只触发一次
	disconnectOnce sync.Once

	// heartbeatInterval/heartbeatTimeout/writeTimeout 为创建连接时的参数快照（创建后只读）
	heartbeatInterval time.Duration
	heartbeatTimeout  time.Duration
	writeTimeout      time.Duration
}

func (c *ConnSocket) RemoteAddr() string {
	if c.conn == nil || c.conn.RemoteAddr() == nil {
		return ""
	}
	return c.conn.RemoteAddr().String()
}

// Disconnect 关闭发送队列，并保证底层 fd 一定被关掉。
//
// 原实现只做 q.Close()，把关 fd 交给 senderRun"排空队列之后"那一步——但队列可能永远
// 排不空：对端停止读取时 senderRun 会一直卡在 conn.Write 上（此前全链路没有任何写
// 超时）。fd 不关 → receiver 继续阻塞在 Read → 断线回调永不触发 → 本端绑定状态不清、
// 对端又被 "already registered" 拒绝重连，**网络分区无法自愈**。因此这里挂一个无条件
// 兜底定时器：给已入队的数据 DisconnectGrace 窗口尽力写出，到点直接关 fd
// （停机时可能丢少量积压，但换来有界、可自愈的停机）。
func (c *ConnSocket) Disconnect() error {
	c.q.Close()
	if c.conn == nil {
		return nil
	}
	conn := c.conn
	// 先设一个很短的写超时：让此刻正卡在 Write 里的 senderRun 尽快失败退出
	_ = conn.SetWriteDeadline(time.Now().Add(DisconnectGrace))
	time.AfterFunc(DisconnectGrace, func() { _ = conn.Close() })
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

// ErrSendQueueFull 表示连接活着但发送侧已经跟不上：帧被丢弃而不是排进无界队列。
var ErrSendQueueFull = errors.New("send queue is full")

// SendData 把一帧交给发送队列。返回 ErrSendQueueFull 表示帧被丢弃（队列有界且已满），
// 返回其他非 nil 表示连接已关闭。两者都不会阻塞调用方——反压以"丢并报错"体现。
func (c *ConnSocket) SendData(data []byte) error {
	if c.q.IsClosed() {
		return errors.New("connection closed")
	}
	if !c.q.Enqueue(data) {
		return ErrSendQueueFull
	}
	return nil
}

func (c *ConnSocket) SetOnDisconnect(onDisconnectFunc netConnect.OnDisconnectFunc) {
	c.onDisconnectFunc = onDisconnectFunc
}

func (c *ConnSocket) SetOnData(onDataFunc netConnect.OnDataFunc) {
	c.onDataFunc = onDataFunc
}

// safeOnData 执行上层数据回调，并把回调内的 panic 转成"连接错误"。
//
// 为什么要在这里兜：读 goroutine 里一次 panic 会击穿整个进程（engine 与上层的
// OnMessage 链路都不 recover）；而即使有 recover，若不走完整的失败路径——
// q.Close() + notifyDisconnect()——就会留下一个 fd 仍开着、断线回调永不触发的
// 半死连接：本端绑定状态不清，对端还被 "already registered" 永久拒绝重连。
// 转成 error 让 receiverRun 按既有失败路径收尾，崩溃变成"断这一条链路"。
//
// 上层应当自己再 recover 一次并记日志（panic 文本只在这里可拿到，engine 层没有
// 日志设施）；本兜底只为不属于本仓库的 engine 使用者存在。
func (c *ConnSocket) safeOnData(data []byte) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("onData callback panic: %v", r)
		}
	}()
	return c.onDataFunc(data)
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
		// n>0 时必须先消费：Read 可以同时返回数据与 io.EOF（对端"发完即关"），
		// 直接按错误退出会丢掉这最后一次里已经完整的帧。
		if n > 0 {
			err = c.safeOnData(buf[:n])
			if err != nil {
				c.q.Close()
				c.notifyDisconnect()
				return
			}
		}
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
			// 本端主动 Disconnect 且队列已排空时走的是这条路径：回调同样必须发出，
			// 否则绑定状态永不清理（notifyDisconnect 幂等，重复调用无副作用）。
			c.notifyDisconnect()
			return
		}
		msg := data
		for len(msg) > 0 {
			if c.writeTimeout > 0 {
				// 写超时：卡住的 writer 是"半死连接"的唯一藏身处——读侧有 heartbeatTimeout
				// 兜底，写侧此前没有任何上界，慢/死对端会让本 goroutine 永久停在 Write，
				// fd 与队列一起长驻，且断线回调不会触发。
				_ = c.conn.SetWriteDeadline(time.Now().Add(c.writeTimeout))
			}
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
