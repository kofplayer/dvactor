package dvactor

import (
	"sync"
	"sync/atomic"
	"time"

	netClient "github.com/kofplayer/dvactor/engine/net/client"
	socketNetConnect "github.com/kofplayer/dvactor/engine/net/connect/socket"
	"github.com/kofplayer/dvactor/protocol"
	"github.com/kofplayer/vactor"
	"google.golang.org/protobuf/proto"
)

// registerResponseTimeout 等待注册响应的超时：防止响应丢失时 client 永久卡死。
const registerResponseTimeout = 10 * time.Second

// 重连退避区间：从 reconnectBackoffMin 起指数增长，封顶 reconnectBackoffMax。
//
// 起点必须**远小于**上层常见的 ConnectTimeout（默认 30s、测试里常用 5s）：用固定的
// 5s 时，"对端比自己晚起几毫秒"这一最常见的启动竞态会让人第一次失败后直接睡满 5s，
// 正好吃掉整个互连超时窗口，表现为集群起不来。指数退避把首轮延迟压到毫秒级，
// 而对真正宕机的节点仍会稳定增长到同样的 5s 上限。
const (
	reconnectBackoffMin = 200 * time.Millisecond
	reconnectBackoffMax = 5 * time.Second
)

// clientStopTimeout 停机时等待重连循环退出的上限。循环里每个阻塞点（拨号、写、
// 队列）现在都有上界，正常应当远快于此；但仍要有超时——无界 Wait 换来的"看起来
// 干净"会在任何一个 goroutine 卡住时把整个停机一起拖死。
const clientStopTimeout = 5 * time.Second

func NewClusterClient(cn *clusterNet, systemId vactor.SystemId) *clusterClient {
	return &clusterClient{
		cn:                   cn,
		systemId:             systemId,
		disconnectChan:       make(chan bool, 1),
		registerResponseChan: make(chan bool, 1),
		closed:               make(chan struct{}),
	}
}

type clusterClient struct {
	cli netClient.NetClient
	wg  sync.WaitGroup
	// retryDelay 只属于重连 goroutine（见 backoff）
	retryDelay           time.Duration
	systemId             vactor.SystemId
	cn                   *clusterNet
	disconnectChan       chan bool
	registerResponseChan chan bool
	closed               chan struct{}
	closeOnce            sync.Once
}

// Stop 终止重连循环并关断当前这一轮的连接（幂等，且有界等待其真正退出）。
//
// 只 close(c.closed) 是不够的：循环可能正停在 Connect() 里（黑洞地址），或者
// 连接仍然 OPEN——停机返回后 TCP 还开着，泄漏的读 goroutine 会继续回 Pong，
// 于是对端把这条链路当作健康并持续向一个已经停机的进程投递。
func (c *clusterClient) Stop() {
	c.closeOnce.Do(func() {
		close(c.closed)
	})
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(clientStopTimeout):
		c.cn.localSystem.LogError("cluster client to system %v did not exit within %v",
			c.systemId, clientStopTimeout)
	}
}

// stopCurrent 关断当前这一轮持有的连接。只在重连循环自己的 goroutine 上调用，
// 因此不需要为 c.cli 再加同步。
func (c *clusterClient) stopCurrent() {
	if c.cli != nil {
		_ = c.cli.Disconnect()
	}
}

// backoff 等待下一轮退避并把它翻倍（封顶 reconnectBackoffMax）；期间收到停止信号则
// 返回 false（调用方应退出）。retryDelay 只被重连 goroutine 读写，无需加锁；
// 连接成功后由 noteConnected 归零。
func (c *clusterClient) backoff() bool {
	c.retryDelay = nextRetryDelay(c.retryDelay)
	d := c.retryDelay
	select {
	case <-time.After(d):
		return true
	case <-c.closed:
		return false
	}
}

// nextRetryDelay 计算下一轮退避时长：首轮取最小值，其后指数增长并封顶。
// 抽成纯函数是为了能不等真实睡眠就把增长序列测全。
func nextRetryDelay(cur time.Duration) time.Duration {
	if cur <= 0 {
		return reconnectBackoffMin
	}
	d := cur * 2
	if d > reconnectBackoffMax {
		return reconnectBackoffMax
	}
	return d
}

// noteConnected 在互连建立后重置退避，使下一次真实断线从最短间隔重新开始。
func (c *clusterClient) noteConnected() { c.retryDelay = 0 }

// drainRegisterResponse 清空注册响应通道中残留的值。
// 每轮重连开始时调用：上一轮迟到的响应若残留，会被本轮误当作"本轮注册成功"，
// 造成未真正注册却判定成功（集群假连通）。
func (c *clusterClient) drainRegisterResponse() {
	select {
	case <-c.registerResponseChan:
	default:
	}
}

func (c *clusterClient) Start() {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		info := c.cn.systemInfos[c.systemId]
		for {
			// 每轮连接前清空上一轮可能残留的注册响应
			c.drainRegisterResponse()
			select {
			case <-c.closed:
				return
			case <-c.disconnectChan:
			default:
			}
			conn := socketNetConnect.NewConnector()
			conn.SetAddress(info.config.Host, info.config.Port)
			cli := netClient.NewNetClient()
			cli.SetConnector(conn)
			cli.SetOnConnect(c.OnConnect)
			cli.SetOnDisconnect(c.OnDisconnect)
			cli.SetOnMessage(func(msgId uint32, data []byte) error {
				return c.cn.dispatchSafely(func() error { return c.OnMessage(msgId, data) },
					"cluster client onMessage", msgId)
			})
			c.cli = cli
			if err := c.cli.Connect(); err != nil {
				c.cn.localSystem.LogError("system %v connect err:%v, wait for retry", info.config.SystemId, err)
				if !c.backoff() {
					return
				}
				continue
			}
			req := &protocol.PkgRegisterSystemReq{
				SystemId:  uint32(c.cn.clusterConfig.LocalSystemId),
				AuthToken: c.cn.clusterConfig.AuthToken,
			}
			data, _ := proto.Marshal(req)
			if err := c.cli.SendMessage(uint32(protocol.PkgType_PkgTypeRegisterSystemReq), data); err != nil {
				_ = c.cli.Disconnect()
				if !c.backoff() {
					return
				}
				continue
			}
			// 等待注册响应：带超时，防止响应丢失导致集群启动永久挂起
			select {
			case <-c.closed:
				// 此刻 TCP 连接已经建立、只是还没等到响应：不关就会留下一条
				// 对端视为"已接入"的孤儿连接，且 Stop 之后它还会继续回 Pong
				c.stopCurrent()
				return
			case <-c.disconnectChan:
				if !c.backoff() {
					return
				}
				continue
			case <-time.After(registerResponseTimeout):
				c.cn.localSystem.LogError("system %v register response timeout, retry", info.config.SystemId)
				_ = c.cli.Disconnect()
				if !c.backoff() {
					return
				}
				continue
			case succ := <-c.registerResponseChan:
				if !succ {
					_ = c.cli.Disconnect()
					if !c.backoff() {
						return
					}
					continue
				}
			}

			info.lock.Lock()
			info.cli = c.cli
			info.lock.Unlock()
			c.noteConnected()

			// ready
			atomic.AddInt32(&c.cn.connectedSystemCount, 1)
			c.cn.localSystem.LogInfo("system %v connected", info.config.SystemId)
			// 注册成功同样意味着连接是"新建立"的：触发本机代理 watch 刷新
			// （覆盖分区期间发出的订阅、以及历史连接上丢失的订阅状态）
			c.cn.localSystem.onSystemReconnected(c.systemId)
			select {
			case <-c.closed:
				c.stopCurrent()
				return
			case <-c.disconnectChan:
			}

			c.cn.localSystem.LogInfo("system %v disconnected", info.config.SystemId)
			info.lock.Lock()
			info.cli = nil
			c.cli = nil
			info.lock.Unlock()

			atomic.AddInt32(&c.cn.connectedSystemCount, -1)
			if !c.backoff() {
				return
			}
		}
	}()
}

func (c *clusterClient) OnConnect() {
}

func (c *clusterClient) OnDisconnect() {
	// 非阻塞：通道已有未消费的断开信号时，绝不能卡住网络回调 goroutine
	select {
	case c.disconnectChan <- true:
	default:
	}
}

func (c *clusterClient) OnMessage(msgId uint32, data []byte) error {
	switch protocol.PkgType(msgId) {
	case protocol.PkgType_PkgTypeRegisterSystemRsp:
		rsp := &protocol.PkgRegisterSystemRsp{}
		succ := proto.Unmarshal(data, rsp) == nil && rsp.ErrorCode == protocol.ErrorCode_ErrorCodeSuccess
		// 非阻塞：主循环可能已因超时离开本轮等待，阻塞写会卡死网络回调 goroutine
		select {
		case c.registerResponseChan <- succ:
		default:
		}
		return nil
	default:
		return c.cn.OnMessage(msgId, data)
	}
}
