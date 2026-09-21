package netSession

import (
	"errors"
	"sync"

	netConnect "github.com/kofplayer/dvactor/engine/net/connect"
)

// ErrNoSendMessageFunc 会话尚未设置发送函数（建立过程中被关闭，或引擎用法不对）。
var ErrNoSendMessageFunc = errors.New("session has no SendMessageFunc bound")

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

// netSession 的字段会被三条不同的 goroutine 读写：本连接的 receiver、本连接的
// sender（写失败时也会触发断线回调），以及任意调用 Close/SendMessage 的一方
// （clusterServer.Stop 遍历关闭会话、注册处理回发响应）。早先完全没有同步，
// interface 字段的撕裂读会让 `bind.(*systemInfo)` 判错，绑定因此永不清理。
//
// 锁只保护字段读写，**不跨 SendMessage 持有**：真正的 socket 写入可能在队列上
// 等待，持锁会放大成整条会话的串行化。
type netSession struct {
	id              SessionID
	mu              sync.RWMutex
	bindObject      interface{}
	conn            netConnect.Conn
	sendMessageFunc SendMessageFunc
}

func (s *netSession) GetBindObject() interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bindObject
}

func (s *netSession) SetBindObject(bindObject interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bindObject = bindObject
}

func (s *netSession) Init() error {
	return nil
}

func (s *netSession) GetID() SessionID {
	return s.id // 创建后只读
}

func (s *netSession) GetConn() netConnect.Conn {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.conn
}

func (s *netSession) SetConn(conn netConnect.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conn = conn
}

func (s *netSession) SetSendMessageFunc(sendMessageFunc SendMessageFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sendMessageFunc = sendMessageFunc
}

func (s *netSession) SendMessage(msgId uint32, data []byte) error {
	s.mu.RLock()
	f := s.sendMessageFunc
	s.mu.RUnlock()
	if f == nil {
		return ErrNoSendMessageFunc
	}
	return f(msgId, data)
}

// Close 关闭底层连接。幂等：底层 Disconnect 自身可重复调用，因此这里不区分首次
// 与重复——调用方遍布停机与断线回调路径，把"已经关过"当错误上报只会制造噪声。
// 刻意不把 conn 置 nil：置空会让并发的 GetConn/SendMessage 看到"没有连接"，
// 而连接对象自身的 Close 本来就是幂等的。
func (s *netSession) Close() error {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return nil
	}
	return conn.Disconnect()
}
