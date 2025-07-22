package dvactor

import (
	"fmt"
	"sync/atomic"
	"time"

	netClient "github.com/kofplayer/dvactor/engine/net/client"
	socketNetConnect "github.com/kofplayer/dvactor/engine/net/connect/socket"
	"github.com/kofplayer/dvactor/protocol"
	"github.com/kofplayer/vactor"
	"google.golang.org/protobuf/proto"
)

func NewClusterClient(cn *clusterNet, systemId vactor.SystemId) *clusterClient {
	return &clusterClient{
		cn:                   cn,
		systemId:             systemId,
		disconnectChan:       make(chan bool, 1),
		registerResponseChan: make(chan bool, 1),
	}
}

type clusterClient struct {
	cli                  netClient.NetClient
	systemId             vactor.SystemId
	cn                   *clusterNet
	disconnectChan       chan bool
	registerResponseChan chan bool
}

func (c *clusterClient) Start() {
	go func() {
		info := c.cn.systemInfos[c.systemId]
		for {
			select {
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
				fmt.Printf("system %v connect err:%v, wait for retry\n", info.config.SystemId, err)
				time.Sleep(time.Second * 5)
				continue
			}
			req := &protocol.PkgRegisterSystemReq{
				SystemId: uint32(c.cn.clusterConfig.LocalSystemId),
			}
			data, _ := proto.Marshal(req)
			if err := c.cli.SendMessage(uint32(protocol.PkgType_PkgTypeRegisterSystemReq), data); err != nil {
				c.cli.Disconnect()
				time.Sleep(time.Second * 5)
				continue
			}
			select {
			case <-c.disconnectChan:
				time.Sleep(time.Second * 5)
				continue
			case succ := <-c.registerResponseChan:
				if !succ {
					c.cli.Disconnect()
					time.Sleep(time.Second * 5)
					continue
				}
			}

			info.lock.Lock()
			info.cli = c.cli
			info.lock.Unlock()

			// ready
			atomic.AddInt32(&c.cn.connectedSystemCount, 1)
			fmt.Printf("system %v connected\n", info.config.SystemId)
			<-c.disconnectChan

			fmt.Printf("system %v disconnected\n", info.config.SystemId)
			info.lock.Lock()
			info.cli = nil
			c.cli = nil
			info.lock.Unlock()

			atomic.AddInt32(&c.cn.connectedSystemCount, -1)
			time.Sleep(time.Second * 5)
		}
	}()
}

func (c *clusterClient) OnConnect() {
}

func (c *clusterClient) OnDisconnect() {
	c.disconnectChan <- true
}

func (c *clusterClient) OnMessage(msgId uint32, data []byte) error {
	switch protocol.PkgType(msgId) {
	case protocol.PkgType_PkgTypeRegisterSystemRsp:
		rsp := &protocol.PkgRegisterSystemRsp{}
		if proto.Unmarshal(data, rsp) == nil && rsp.ErrorCode == protocol.ErrorCode_ErrorCodeSuccess {
			c.registerResponseChan <- true
		} else {
			c.registerResponseChan <- false
		}
		return nil
	default:
		return c.cn.OnMessage(msgId, data)
	}
}
