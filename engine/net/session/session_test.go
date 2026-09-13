package netSession_test

import (
	"testing"

	netSession "github.com/kofplayer/dvactor/engine/net/session"
)

func TestSessionMgr(t *testing.T) {
	t.Parallel()

	t.Run("NewSessionUniqueIDs", func(t *testing.T) {
		mgr := netSession.NewSessionMgr()
		ids := map[netSession.SessionID]bool{}
		for i := 0; i < 100; i++ {
			s := mgr.NewSession()
			if s == nil {
				t.Fatal("nil session")
			}
			if ids[s.GetID()] {
				t.Fatalf("duplicate session id %v", s.GetID())
			}
			ids[s.GetID()] = true
		}
	})

	t.Run("GetAndRemove", func(t *testing.T) {
		mgr := netSession.NewSessionMgr()
		s := mgr.NewSession()
		if mgr.GetSession(s.GetID()) != s {
			t.Fatal("get session mismatch")
		}
		mgr.RemoveSession(s.GetID())
		if mgr.GetSession(s.GetID()) != nil {
			t.Fatal("session should be removed")
		}
		if mgr.GetSession(99999) != nil {
			t.Fatal("unknown id should return nil")
		}
	})

	t.Run("TravelSession", func(t *testing.T) {
		mgr := netSession.NewSessionMgr()
		for i := 0; i < 5; i++ {
			mgr.NewSession()
		}
		count := 0
		mgr.TravelSession(func(s netSession.NetSession) bool {
			count++
			return true
		})
		if count != 5 {
			t.Fatalf("traveled %d sessions, want 5", count)
		}

		// 回调返回 false 提前终止
		count = 0
		mgr.TravelSession(func(s netSession.NetSession) bool {
			count++
			return false
		})
		if count != 1 {
			t.Fatalf("early stop traveled %d, want 1", count)
		}
	})
}

func TestSessionBindObjectAndClose(t *testing.T) {
	t.Parallel()

	mgr := netSession.NewSessionMgr()
	s := mgr.NewSession()
	if s.GetBindObject() != nil {
		t.Fatal("bind object should start nil")
	}
	obj := struct{ Name string }{Name: "peer"}
	s.SetBindObject(obj)
	if s.GetBindObject().(struct{ Name string }).Name != "peer" {
		t.Fatal("bind object mismatch")
	}
	// 无连接的 session Close 应安全返回 nil
	if err := s.Close(); err != nil {
		t.Fatalf("close without conn: %v", err)
	}
}
