package server

import (
	"encoding/binary"

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
		var receiveData []byte
		s.SetSendMessageFunc(func(msgId uint32, data []byte) error {
			l := len(data)
			_data := make([]byte, 5, l+5)
			binary.BigEndian.PutUint32(_data[:4], uint32(l))
			_data[4] = uint8(msgId)
			_data = append(_data, data...)
			return conn.SendData(_data)
		})
		conn.SetOnDisconnect(func() {
			ns.onDisconnect(s)
			ns.sessionMgr.RemoveSession(s.GetID())
		})
		conn.SetOnData(func(data []byte) error {
			// len(4) msgId(1) data
			if receiveData == nil {
				receiveData = data
			} else {
				receiveData = append(receiveData, data...)
			}
			for {
				l := uint32(len(receiveData))
				if l < 5 {
					return nil
				}
				dataLen := binary.BigEndian.Uint32(receiveData[0:4])
				msgLen := dataLen + 5
				if l < msgLen {
					return nil
				}
				t := uint32(receiveData[4])
				_data := receiveData[5:msgLen]
				ns.onMessage(s, t, _data)
				receiveData = receiveData[msgLen:]
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
