package dvactor_test

import (
	"fmt"
	"testing"

	"github.com/kofplayer/dvactor"
	"github.com/kofplayer/vactor"
)

// 3 节点配置：类型 120 只在节点 1、2 声明；节点 3 不声明。
// 仅用于放置哈希验证，不会真正启动网络。
func newPlacementSystem(t *testing.T) dvactor.ClusterSystem {
	t.Helper()
	return dvactor.NewSystem(&dvactor.ClusterConfig{
		LocalSystemId: 1,
		SystemConfigs: []*dvactor.SystemConfig{
			{SystemId: 1, Host: "127.0.0.1", Port: 19001, ActorTypes: []vactor.ActorType{120}},
			{SystemId: 2, Host: "127.0.0.1", Port: 19002, ActorTypes: []vactor.ActorType{120}},
			{SystemId: 3, Host: "127.0.0.1", Port: 19003},
		},
	}, func(sc *vactor.SystemConfig) {
		sc.LogFunc = func(vactor.LogLevel, string, ...interface{}) {}
	})
}

func TestPlacementDeterministicAndInCandidates(t *testing.T) {
	s := newPlacementSystem(t)

	seen := map[vactor.SystemId]bool{}
	for i := 0; i < 60; i++ {
		id := vactor.ActorId(fmt.Sprintf("actor-%d", i))
		ref := s.CreateActorRef(120, id)
		if ref.GetSystemId() != 1 && ref.GetSystemId() != 2 {
			t.Fatalf("placement %v outside candidate set {1,2}", ref.GetSystemId())
		}
		if ref.GetGroupSlot() == 0 {
			t.Fatal("group slot must never be 0")
		}
		seen[ref.GetSystemId()] = true
		// 确定性：同一 actorId 重复创建结果一致
		again := s.CreateActorRef(120, id)
		if again.GetSystemId() != ref.GetSystemId() || again.GetGroupSlot() != ref.GetGroupSlot() {
			t.Fatalf("placement not deterministic for %v", id)
		}
	}
	if len(seen) < 2 {
		t.Fatal("placement hash has no spread across candidate nodes")
	}
}

func TestPlacementFallbackToLocalWhenUndeclared(t *testing.T) {
	s := newPlacementSystem(t)
	ref := s.CreateActorRef(999, "x") // 未在任何节点声明
	if ref.GetSystemId() != 1 {
		t.Fatalf("should fallback to local system 1, got %v", ref.GetSystemId())
	}
}

func TestPlacementExplicitSystemIdRespected(t *testing.T) {
	s := newPlacementSystem(t)
	ref := s.CreateActorRefEx(3, 120, "x")
	if ref.GetSystemId() != 3 {
		t.Fatalf("explicit systemId ignored: %v", ref.GetSystemId())
	}
}

func TestRegisterActorTypeRequiresDvactorStart(t *testing.T) {
	s := newPlacementSystem(t)
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for actorType < dvactor.ActorTypeStart(20)")
		}
	}()
	s.RegisterActorType(15, nil) // 15 < 20，必须 panic
}

// 回归测试：向集群配置中未声明的 SystemId 发消息必须返回错误，不能 panic。
// 系统外（main/业务 goroutine）调用没有 recover 兜底，panic 会直接杀进程。
func TestSendToUndeclaredSystemIdReturnsError(t *testing.T) {
	s := newPlacementSystem(t)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic on undeclared systemId: %v", r)
		}
	}()
	ref := s.CreateActorRefEx(99, 120, "x") // 99 未在任何 SystemConfig 中声明
	err := s.BatchSend([]vactor.ActorRef{ref}, []interface{}{nil})
	if err == nil {
		t.Fatal("expected error for undeclared systemId, got nil")
	}
	if err.Code() != dvactor.ErrorCodeUnknownSystem {
		t.Fatalf("expected ErrorCodeUnknownSystem, got code=%v err=%v", err.Code(), err)
	}
}

// 放置哈希必须均匀铺开：历史上用的交替 XOR 在结构化 id 下会把大量 actor
// 塞进同一节点 / 同一槽位（实测 2 万个 userN 只落 762 个槽位）。
func TestPlacementHashSpreadsEvenly(t *testing.T) {
	const n = 20000
	s := newPlacementSystem(t)
	byNode := map[vactor.SystemId]int{}
	slots := map[vactor.GroupSlot]bool{}
	for i := 1; i <= n; i++ {
		ref := s.CreateActorRef(120, vactor.ActorId(fmt.Sprintf("user%d", i)))
		byNode[ref.GetSystemId()]++
		slots[ref.GetGroupSlot()] = true
	}
	if len(byNode) != 2 {
		t.Fatalf("actors should spread over both candidate nodes, got %v", byNode)
	}
	for node, c := range byNode {
		// 两个候选节点期望各约一半，允许 45%~55%
		if c < n*45/100 || c > n*55/100 {
			t.Errorf("node %v got %d/%d actors, too skewed", node, c, n)
		}
	}
	if len(slots) < n/2 {
		t.Errorf("only %d distinct group slots for %d actors, hash spread too poor", len(slots), n)
	}
}
