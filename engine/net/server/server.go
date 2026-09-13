package server

import (
	netConnect "github.com/kofplayer/dvactor/engine/net/connect"
	netSession "github.com/kofplayer/dvactor/engine/net/session"
)

func NewNetServer() NetServer {
	v := new(netServer)
	v.sessionMgr = netSession.NewSessionMgr()
	return v
}

type NetServer interface {
	SetAcceptor(acceptor netConnect.Acceptor)
	SetOnConnect(func(netSession.NetSession))
	SetOnDisconnect(func(netSession.NetSession))
	SetOnMessage(func(s netSession.NetSession, msgId uint32, data []byte) error)
	Start() error
	Stop() error
	GetSessionMgr() netSession.SessionMgr
}

type netServer struct {
	acceptor     netConnect.Acceptor
	onConnect    func(netSession.NetSession)
	onDisconnect func(netSession.NetSession)
	onMessage    func(s netSession.NetSession, t uint32, data []byte) error
	sessionMgr   netSession.SessionMgr
}

func (ns *netServer) SetAcceptor(acceptor netConnect.Acceptor) {
	ns.acceptor = acceptor
}

func (ns *netServer) SetOnConnect(onConnect func(netSession.NetSession)) {
	ns.onConnect = onConnect
}

func (ns *netServer) SetOnDisconnect(onDisconnect func(netSession.NetSession)) {
	ns.onDisconnect = onDisconnect
}

func (ns *netServer) SetOnMessage(onMessage func(s netSession.NetSession, t uint32, data []byte) error) {
	ns.onMessage = onMessage
}

func (ns *netServer) Start() error {
	ns.acceptor.SetOnAccept(func(conn netConnect.Conn) {
		s := ns.sessionMgr.NewSession()
		s.SetConn(conn)
		var splitter netConnect.PacketSplitter
		s.SetSendMessageFunc(func(msgId uint32, data []byte) error {
			pkt, err := netConnect.PackMessage(msgId, data)
			if err != nil {
				return err
			}
			return conn.SendData(pkt)
		})
		conn.SetOnDisconnect(func() {
			if ns.onDisconnect != nil {
				ns.onDisconnect(s)
			}
			ns.sessionMgr.RemoveSession(s.GetID())
		})
		conn.SetOnData(func(data []byte) error {
			splitter.Append(data)
			for {
				msgId, payload, ok, err := splitter.Next()
				if err != nil {
					// 帧长非法，字节流已不可信，断开连接
					return err
				}
				if !ok {
					return nil
				}
				// 引擎层心跳在回调前拦截，不进入业务层
				if msgId == netConnect.HeartbeatMsgIdPing {
					if pkt, err := netConnect.PackMessage(netConnect.HeartbeatMsgIdPong, nil); err == nil {
						_ = conn.SendData(pkt)
					}
					continue
				}
				if msgId == netConnect.HeartbeatMsgIdPong {
					continue
				}
				if ns.onMessage == nil {
					continue
				}
				if err := ns.onMessage(s, msgId, payload); err != nil {
					// 业务回调错误触发断线（clusterServer 依赖该语义，并可自行提前 Close）
					return err
				}
			}
		})
		if ns.onConnect != nil {
			ns.onConnect(s)
		}
	})
	return ns.acceptor.Start()
}

func (ns *netServer) Stop() error {
	return ns.acceptor.Stop()
}

func (ns *netServer) GetSessionMgr() netSession.SessionMgr {
	return ns.sessionMgr
}
