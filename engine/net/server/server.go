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
			ns.onDisconnect(s)
			ns.sessionMgr.RemoveSession(s.GetID())
		})
		conn.SetOnData(func(data []byte) error {
			splitter.Append(data)
			for {
				msgId, payload, ok := splitter.Next()
				if !ok {
					return nil
				}
				ns.onMessage(s, msgId, payload)
			}
		})
		ns.onConnect(s)
	})
	return ns.acceptor.Start()
}

func (ns *netServer) Stop() error {
	return ns.acceptor.Stop()
}

func (ns *netServer) GetSessionMgr() netSession.SessionMgr {
	return ns.sessionMgr
}
