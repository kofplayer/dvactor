package netSession_test

import (
	"sync"
	"testing"

	netConnect "github.com/kofplayer/dvactor/engine/net/connect"
	netSession "github.com/kofplayer/dvactor/engine/net/session"
)

type raceConn struct{}

func (c *raceConn) RemoteAddr() string { return "x" }
func (c *raceConn) Disconnect() error  { return nil }
func (c *raceConn) SendData([]byte) error {
	return nil
}
func (c *raceConn) SetOnDisconnect(netConnect.OnDisconnectFunc) {}
func (c *raceConn) SetOnData(netConnect.OnDataFunc)             {}

// 会话字段会被三条 goroutine 读写：本连接的 receiver、本连接的 sender（写失败时
// 也会走断线回调 → 读 bindObject），以及任意调用 Close/SendMessage 的一方。
// 这里并发压一遍，在 -race 下验证同步到位（修复前 bindObject/conn 是裸字段）。
func TestSessionConcurrentAccess(t *testing.T) {
	mgr := netSession.NewSessionMgr()
	s := mgr.NewSession()
	s.SetConn(&raceConn{})
	s.SetSendMessageFunc(func(uint32, []byte) error { return nil })

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for k := 0; k < 200; k++ {
				switch (i + k) % 5 {
				case 0:
					s.SetBindObject(&struct{ n int }{n: k})
				case 1:
					_ = s.GetBindObject()
				case 2:
					_ = s.SendMessage(1, nil)
				case 3:
					_ = s.GetConn()
				case 4:
					_ = s.Close()
				}
			}
		}(i)
	}
	wg.Wait()
}

// Count 是连接数上限的判据，本包内直接覆盖（跨包调用的覆盖率不会被计入）。
func TestSessionMgrCountTracksRegistrations(t *testing.T) {
	mgr := netSession.NewSessionMgr()
	if mgr.Count() != 0 {
		t.Fatalf("fresh manager count = %d, want 0", mgr.Count())
	}
	a := mgr.NewSession()
	b := mgr.NewSession()
	if mgr.Count() != 2 {
		t.Fatalf("count = %d, want 2", mgr.Count())
	}
	mgr.RemoveSession(a.GetID())
	if mgr.Count() != 1 {
		t.Fatalf("count = %d, want 1 after removing one", mgr.Count())
	}
	mgr.RemoveSession(b.GetID())
	mgr.RemoveSession(9999) // 移除不存在的 id 不应 panic
	if mgr.Count() != 0 {
		t.Fatalf("count = %d, want 0", mgr.Count())
	}
}
