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

// reconnectBackoff 断线后的重连退避间隔。
const reconnectBackoff = 5 * time.Second

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
	cli                  netClient.NetClient
	systemId             vactor.SystemId
	cn                   *clusterNet
	disconnectChan       chan bool
	registerResponseChan chan bool
	closed               chan struct{}
	closeOnce            sync.Once
}

// Stop 终止重连循环（幂等）。正在进行的连接尝试会在下一个检查点退出。
func (c *clusterClient) Stop() {
	c.closeOnce.Do(func() {
		close(c.closed)
	})
}

// backoff 等待 d 时间；期间收到停止信号则返回 false（调用方应退出）。
func (c *clusterClient) backoff(d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-c.closed:
		return false
	}
}

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
	go func() {
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
			cli.SetOnMessage(c.OnMessage)
			c.cli = cli
			if err := c.cli.Connect(); err != nil {
				c.cn.localSystem.LogError("system %v connect err:%v, wait for retry", info.config.SystemId, err)
				if !c.backoff(reconnectBackoff) {
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
				c.cli.Disconnect()
				if !c.backoff(reconnectBackoff) {
					return
				}
				continue
			}
			// 等待注册响应：带超时，防止响应丢失导致集群启动永久挂起
			select {
			case <-c.closed:
				return
			case <-c.disconnectChan:
				if !c.backoff(reconnectBackoff) {
					return
				}
				continue
			case <-time.After(registerResponseTimeout):
				c.cn.localSystem.LogError("system %v register response timeout, retry", info.config.SystemId)
				c.cli.Disconnect()
				if !c.backoff(reconnectBackoff) {
					return
				}
				continue
			case succ := <-c.registerResponseChan:
				if !succ {
					c.cli.Disconnect()
					if !c.backoff(reconnectBackoff) {
						return
					}
					continue
				}
			}

			info.lock.Lock()
			info.cli = c.cli
			info.lock.Unlock()

			// ready
			atomic.AddInt32(&c.cn.connectedSystemCount, 1)
			c.cn.localSystem.LogInfo("system %v connected", info.config.SystemId)
			// 注册成功同样意味着连接是"新建立"的：触发本机代理 watch 刷新
			// （覆盖分区期间发出的订阅、以及历史连接上丢失的订阅状态）
			c.cn.localSystem.onSystemReconnected(c.systemId)
			select {
			case <-c.closed:
				return
			case <-c.disconnectChan:
			}

			c.cn.localSystem.LogInfo("system %v disconnected", info.config.SystemId)
			info.lock.Lock()
			info.cli = nil
			c.cli = nil
			info.lock.Unlock()

			atomic.AddInt32(&c.cn.connectedSystemCount, -1)
			if !c.backoff(reconnectBackoff) {
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
