package netSession

import (
	"testing"
	"time"
)

// TravelSession 的回调不得在读锁内执行：回调里 Close 会话/登记新会话都要抢写锁，
// 同一 goroutine 上 RLock→Lock 是自死锁（Go 的 RWMutex 不允许读锁升级）。
// 这里让回调直接调用会抢写锁的方法——修复前本用例会挂到超时。
func TestTravelSessionCallbackMayMutateSessionMgr(t *testing.T) {
	mgr := NewSessionMgr()
	originals := make([]SessionID, 0, 3)
	for i := 0; i < 3; i++ {
		originals = append(originals, mgr.NewSession().GetID())
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		mgr.TravelSession(func(s NetSession) bool {
			// 回调内部访问管理器：快照实现下这些调用都在锁外，不会互锁
			if got := mgr.GetSession(s.GetID()); got == nil {
				t.Errorf("GetSession(%v) inside callback returned nil", s.GetID())
			}
			mgr.RemoveSession(s.GetID())
			mgr.NewSession()
			return true
		})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("TravelSession deadlocked: the callback ran while holding the read lock")
	}

	// 每个原会话都被移除、同时新增一个：总数不变（3），但 id 必须全换过
	seen := make(map[SessionID]bool)
	count := 0
	mgr.TravelSession(func(s NetSession) bool {
		count++
		seen[s.GetID()] = true
		return true
	})
	if count != 3 {
		t.Fatalf("sessions after traversal = %d, want 3", count)
	}
	for _, id := range originals {
		if seen[id] {
			t.Fatalf("original session %v survived: the callback's RemoveSession had no effect", id)
		}
	}
}

// 回调返回 false 必须立刻停止遍历（短路语义不能因为改成快照而丢失）。
func TestTravelSessionStopsOnFalse(t *testing.T) {
	mgr := NewSessionMgr()
	for i := 0; i < 5; i++ {
		mgr.NewSession()
	}
	visited := 0
	mgr.TravelSession(func(s NetSession) bool {
		visited++
		return visited < 2
	})
	if visited != 2 {
		t.Fatalf("visited = %d, want 2", visited)
	}
}
