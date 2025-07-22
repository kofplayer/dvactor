package netSession

import (
	"sync"
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
}

type sessionMgr struct {
	lock     sync.RWMutex
	sessions map[SessionID]*netSession
	genUId   SessionID
}

func (m *sessionMgr) NewSession() NetSession {
	v := new(netSession)
	v.Init()
	m.genUId++
	v.id = m.genUId
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

func (m *sessionMgr) TravelSession(f func(s NetSession) bool) {
	m.lock.RLock()
	defer m.lock.RUnlock()
	for _, v := range m.sessions {
		if !f(v) {
			break
		}
	}
}
