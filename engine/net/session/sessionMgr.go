package netSession

import (
	"sync"
	"sync/atomic"
)

func NewSessionMgr() SessionMgr {
	v := new(sessionMgr)
	v.sessions = make(map[SessionID]*netSession)
	return v
}

type SessionMgr interface {
	NewSession() NetSession
	RemoveSession(sID SessionID)
	GetSession(sID SessionID) NetSession
	TravelSession(f func(s NetSession) bool)
	Count() int
}

type sessionMgr struct {
	lock     sync.RWMutex
	sessions map[SessionID]*netSession
	genUId   atomic.Uint64
}

func (m *sessionMgr) NewSession() NetSession {
	v := new(netSession)
	_ = v.Init()
	v.id = SessionID(m.genUId.Add(1))
	m.lock.Lock()
	defer m.lock.Unlock()
	m.sessions[v.id] = v
	return v
}

func (m *sessionMgr) RemoveSession(sID SessionID) {
	m.lock.Lock()
	defer m.lock.Unlock()
	delete(m.sessions, sID)
}

func (m *sessionMgr) GetSession(sID SessionID) NetSession {
	m.lock.RLock()
	defer m.lock.RUnlock()
	v, ok := m.sessions[sID]
	if !ok {
		return nil
	}
	return v
}

// TravelSession 遍历所有会话。先在读锁下拍快照、**解锁后**再执行回调：
// 回调里通常会 Close 会话，而 Close 可能牵连 RemoveSession/NewSession 抢写锁——
// 持 RLock 跑回调不仅会阻塞这些写操作，一旦同一 goroutine 上发生 RLock→Lock
// 就是自死锁（Go 的 RWMutex 不允许读锁升级）。
// Count 返回当前登记的会话数（用于连接数上限判断）。
func (m *sessionMgr) Count() int {
	m.lock.RLock()
	defer m.lock.RUnlock()
	return len(m.sessions)
}

func (m *sessionMgr) TravelSession(f func(s NetSession) bool) {
	m.lock.RLock()
	snapshot := make([]NetSession, 0, len(m.sessions))
	for _, v := range m.sessions {
		snapshot = append(snapshot, v)
	}
	m.lock.RUnlock()

	for _, s := range snapshot {
		if !f(s) {
			break
		}
	}
}
