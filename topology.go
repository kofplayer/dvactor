package dvactor

// clusterTopology 描述本节点在"按 SystemConfigs 列表顺序构成的单向连接"中的角色。
//
// 组网规则（所有节点持有完全一致的 SystemConfigs，各自按下标算出自己的角色）：
//   - 下标比自己大的节点：等待对方连入 → 本节点需要监听；
//   - 下标比自己小的节点：本节点主动去连对方。
//
// 于是列表首节点纯 server、末节点纯 client、中间节点两者兼备，整体构成全互联。
// 这套判定此前散落在 NewClusterNet 的循环状态与 start() 的分支里，
// 拆成纯函数后可以脱离网络直接验证多节点形状。
type clusterTopology struct {
	// localIndex 本节点在 SystemConfigs 中的下标。
	localIndex int
	// nodeCount SystemConfigs 的节点总数。
	nodeCount int
	// listen 本节点是否需要启动 server 监听。
	listen bool
	// dialIndexes 本节点需要主动连接的节点下标，按升序排列。
	dialIndexes []int
}

// computeTopology 按 localIndex / nodeCount 算出本节点的组网角色。
// 纯计算：不读配置、不碰网络，可供直测。
func computeTopology(localIndex, nodeCount int) clusterTopology {
	t := clusterTopology{
		localIndex: localIndex,
		nodeCount:  nodeCount,
		// 列表末节点没有比自己更大的下标，无需监听
		listen: localIndex < nodeCount-1,
	}
	if localIndex > 0 {
		t.dialIndexes = make([]int, 0, localIndex)
		for i := 0; i < localIndex; i++ {
			t.dialIndexes = append(t.dialIndexes, i)
		}
	}
	return t
}

// weDial 判断下标为 index 的节点是否由本节点主动连接。
// 等价于 index < localIndex——这个名字就是 systemInfo.weDial 的语义来源。
func (t clusterTopology) weDial(index int) bool {
	return index < t.localIndex
}
