package netSession

import (
	netConnect "github.com/kofplayer/dvactor/engine/net/connect"
)

// SessionID 会话编号。取 uint64：uint32 在长跑进程中回绕后会与新会话撞号。
type SessionID uint64

type SendMessageFunc func(msgId uint32, data []byte) error

type NetSession interface {
	GetID() SessionID
	GetConn() netConnect.Conn
	SetConn(conn netConnect.Conn)
	SetSendMessageFunc(sendMessageFunc SendMessageFunc)
	SendMessage(msgId uint32, data []byte) error
	GetBindObject() interface{}
	SetBindObject(interface{})
	Close() error
}

type netSession struct {
	id              SessionID
	bindObject      interface{}
	conn            netConnect.Conn
	sendMessageFunc SendMessageFunc
}

func (s *netSession) GetBindObject() interface{} {
	return s.bindObject
}

func (s *netSession) SetBindObject(bindObject interface{}) {
	s.bindObject = bindObject
}

func (s *netSession) Init() error {
	return nil
}

func (s *netSession) GetID() SessionID {
	return s.id
}

func (s *netSession) GetConn() netConnect.Conn {
	return s.conn
}

func (s *netSession) SetConn(conn netConnect.Conn) {
	s.conn = conn
}

func (s *netSession) SetSendMessageFunc(sendMessageFunc SendMessageFunc) {
	s.sendMessageFunc = sendMessageFunc
}

func (s *netSession) SendMessage(msgId uint32, data []byte) error {
	return s.sendMessageFunc(msgId, data)
}

func (s *netSession) Close() error {
	if s.conn != nil {
		err := s.conn.Disconnect()
		s.conn = nil
		return err
	}
	return nil
}
