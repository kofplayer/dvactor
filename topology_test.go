package dvactor

import (
	"slices"
	"testing"

	"github.com/kofplayer/vactor"
)

// 组网角色的显式形状：列表首节点纯 server、末节点纯 client、中间两者兼备。
func TestComputeTopologyShapes(t *testing.T) {
	cases := []struct {
		local, total int
		listen       bool
		dial         []int
	}{
		{0, 1, false, nil},            // 单节点：既不监听也不外连
		{0, 2, true, nil},             // 双节点里的第一个：纯 server
		{1, 2, false, []int{0}},       // 双节点里的第二个：纯 client
		{0, 3, true, nil},             // 三节点：首
		{1, 3, true, []int{0}},        // 三节点：中
		{2, 3, false, []int{0, 1}},    // 三节点：末
		{0, 4, true, nil},             // 四节点：首
		{2, 4, true, []int{0, 1}},     // 四节点：中
		{3, 4, false, []int{0, 1, 2}}, // 四节点：末
	}
	for _, c := range cases {
		got := computeTopology(c.local, c.total)
		if got.listen != c.listen {
			t.Errorf("local=%d/%d: listen = %v, want %v", c.local, c.total, got.listen, c.listen)
		}
		if !slices.Equal(got.dialIndexes, c.dial) {
			t.Errorf("local=%d/%d: dial = %v, want %v", c.local, c.total, got.dialIndexes, c.dial)
		}
		// weDial 与 dialIndexes 必须描述同一件事
		for i := 0; i < c.total; i++ {
			if want := slices.Contains(c.dial, i); got.weDial(i) != want {
				t.Errorf("local=%d/%d: weDial(%d) = %v, want %v", c.local, c.total, i, got.weDial(i), want)
			}
		}
	}
}

// 全互联的不变量：n 个节点两两之间恰好由一方主动连接，连接总数 = C(n,2)。
// 这条性质一旦被破坏，就会出现"谁都不连"（分区）或"都去连"（重复会话）的拓扑。
func TestComputeTopologyFormsFullMesh(t *testing.T) {
	for n := 1; n <= 8; n++ {
		total := 0
		for local := 0; local < n; local++ {
			topo := computeTopology(local, n)
			total += len(topo.dialIndexes)
			if want := local < n-1; topo.listen != want {
				t.Errorf("n=%d local=%d: listen = %v, want %v", n, local, topo.listen, want)
			}
			// 只连比自己小的下标，且升序
			if !slices.IsSorted(topo.dialIndexes) {
				t.Errorf("n=%d local=%d: dial indexes not sorted: %v", n, local, topo.dialIndexes)
			}
		}
		if want := n * (n - 1) / 2; total != want {
			t.Fatalf("n=%d: total dials = %d, want %d", n, total, want)
		}
		// 每一对 (i,j) 恰好一方主动连另一方
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				if computeTopology(i, n).weDial(j) == computeTopology(j, n).weDial(i) {
					t.Fatalf("n=%d pair (%d,%d): direction is not unique", n, i, j)
				}
			}
		}
	}
}

// NewClusterNet 必须把 topology 的判定如实装配到每个 systemInfo 上。
func TestNewClusterNetAssignsWeDial(t *testing.T) {
	configs := []*SystemConfig{
		{SystemId: 1, Host: "h1", Port: 1001},
		{SystemId: 2, Host: "h2", Port: 1002},
		{SystemId: 3, Host: "h3", Port: 1003},
	}
	for local := 0; local < len(configs); local++ {
		cn := NewClusterNet(nil, &ClusterConfig{
			LocalSystemId: configs[local].SystemId,
			SystemConfigs: configs,
		})
		if want := local < len(configs)-1; cn.topology.listen != want {
			t.Errorf("local=%d: listen = %v, want %v", local, cn.topology.listen, want)
		}
		if got, want := len(cn.topology.dialIndexes), local; got != want {
			t.Errorf("local=%d: dial count = %d, want %d", local, got, want)
		}
		for i, sc := range configs {
			info := cn.systemInfos[sc.SystemId]
			if info == nil {
				t.Fatalf("local=%d: systemInfo for %d missing", local, sc.SystemId)
			}
			if info.weDial != (i < local) {
				t.Errorf("local=%d node %d: weDial = %v, want %v", local, sc.SystemId, info.weDial, i < local)
			}
		}
	}
}

// 本地 SystemId 重复时取第一个匹配项（validateClusterConfig 会拒绝这种配置，
// 这里固化"取首个"的语义，避免将来改成取末个而不自知）。
func TestNewClusterNetUsesFirstMatchingLocalIndex(t *testing.T) {
	configs := []*SystemConfig{
		{SystemId: 1, Host: "h1", Port: 1001},
		{SystemId: 9, Host: "h9", Port: 1009},
		{SystemId: 2, Host: "h2", Port: 1002},
	}
	cn := NewClusterNet(nil, &ClusterConfig{
		LocalSystemId: 9,
		SystemConfigs: configs,
	})
	if cn.topology.localIndex != 1 {
		t.Fatalf("localIndex = %d, want 1", cn.topology.localIndex)
	}
	if cn.systemInfos[vactor.SystemId(1)].weDial != true || cn.systemInfos[vactor.SystemId(2)].weDial != false {
		t.Fatal("weDial flags do not follow the resolved local index")
	}
}
