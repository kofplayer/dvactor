// Package testutil 提供 dvactor 的集群测试辅助：在当前进程内启动 N 个节点
// 组成全互联集群（127.0.0.1 临时端口），并附加日志捕获与失败排查输出。
package testutil

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kofplayer/dvactor"
	"github.com/kofplayer/vactor"
	vt "github.com/kofplayer/vactor/testutil"
	"google.golang.org/protobuf/proto"
)

// ClusterOpt 定制集群级 ClusterConfig（所有节点共享的配置项）。
type ClusterOpt func(*dvactor.ClusterConfig)

// WithAuthToken 为全部节点设置注册握手共享密钥。
func WithAuthToken(token string) ClusterOpt {
	return func(c *dvactor.ClusterConfig) { c.AuthToken = token }
}

// NodeSpec 描述一个集群节点。
type NodeSpec struct {
	// ActorTypes 该节点声明可放置的 actor 类型（决定放置哈希的候选节点集）。
	ActorTypes []vactor.ActorType
	// Register 在 Start 之前完成 actor 类型与消息类型注册，可为 nil。
	Register func(s dvactor.ClusterSystem)
}

// Cluster 进程内集群句柄。
type Cluster struct {
	Nodes   []dvactor.ClusterSystem
	systems []*vt.TestSystem
}

// StartCluster 在当前进程内并发启动 len(specs) 个节点并阻塞等待全互联。
// 节点 i 的 SystemId 为 i+1，监听 127.0.0.1 临时端口。超时 30s Fatal。
// Stop 只停 vactor 层（dvactor 当前无集群停机接口，网络 goroutine 随进程退出）。
func StartCluster(t *testing.T, specs []NodeSpec, opts ...ClusterOpt) *Cluster {
	t.Helper()
	n := len(specs)
	if n == 0 {
		t.Fatal("need at least one node")
	}
	ports := vt.FreePorts(t, n)
	id := func(i int) vactor.SystemId { return vactor.SystemId(i + 1) }

	cfgs := make([]*dvactor.SystemConfig, n)
	for i := 0; i < n; i++ {
		cfgs[i] = &dvactor.SystemConfig{
			SystemId:   id(i),
			Host:       "127.0.0.1",
			Port:       uint16(ports[i]),
			ActorTypes: specs[i].ActorTypes,
		}
	}

	nodes := make([]dvactor.ClusterSystem, n)
	systems := make([]*vt.TestSystem, n)
	for i := 0; i < n; i++ {
		clusterCfg := &dvactor.ClusterConfig{
			LocalSystemId:  id(i),
			ConnectTimeout: 15 * time.Second,
			SystemConfigs:  cfgs,
		}
		for _, opt := range opts {
			opt(clusterCfg)
		}
		logs := vt.NewLogBuffer()
		s := dvactor.NewSystem(clusterCfg, func(sc *vactor.SystemConfig) {
			sc.TickInterval = 10 * time.Millisecond
			sc.DefaultStopInterval = 0 // 测试期间不闲置回收
			sc.LogFunc = logs.LogFunc
		})
		if specs[i].Register != nil {
			specs[i].Register(s)
		}
		nodes[i] = s
		systems[i] = vt.WrapSystem(s, logs)
	}

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("node %d start panic: %v", i+1, r)
				}
			}()
			nodes[i].Start()
		}(i)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("cluster start timeout, logs:\n%s", dumpClusterLogs(systems))
	}

	c := &Cluster{Nodes: nodes, systems: systems}
	t.Cleanup(c.Stop)
	t.Cleanup(func() {
		if t.Failed() {
			for i, s := range systems {
				t.Logf("---- node %d logs ----", i+1)
				s.DumpLogs(t)
			}
		}
	})
	return c
}

func dumpClusterLogs(systems []*vt.TestSystem) string {
	out := ""
	for i, s := range systems {
		out += fmt.Sprintf("---- node %d ----\n", i+1)
		for _, l := range s.Logs() {
			out += l + "\n"
		}
	}
	return out
}

// Node 返回第 i 个节点（0 起）。
func (c *Cluster) Node(i int) dvactor.ClusterSystem { return c.Nodes[i] }

// TestSystem 返回第 i 个节点的日志捕获包装。
func (c *Cluster) TestSystem(i int) *vt.TestSystem { return c.systems[i] }

// NodeLogsContain 判断第 i 个节点是否出现过包含 substr 的日志。
func (c *Cluster) NodeLogsContain(i int, substr string) bool {
	return c.systems[i].LogContains(substr)
}

// RegisterMessageTypeAll 在所有节点注册同一业务消息类型。
func (c *Cluster) RegisterMessageTypeAll(msgType uint32, creator func() proto.Message) {
	for _, n := range c.Nodes {
		n.RegisterMessageType(msgType, creator)
	}
}

// Stop 停止所有节点的 vactor 层（幂等）。
func (c *Cluster) Stop() {
	for _, n := range c.Nodes {
		n.Stop()
	}
}

// StartPartialCluster 启动 specs 中声明的部分节点，但集群配置包含 totalNodes 个
// 节点（其余为"幽灵节点"，永不启动）。用于测试 ConnectTimeout 与不可达节点的发送失败。
// specOf(i) 返回第 i 个节点的 spec；幽灵节点只用其 ActorTypes 声明。
// 返回已启动节点的包装与集群配置，由调用方自行 Start（本函数不启动）。
func BuildClusterConfig(t *testing.T, totalNodes int, actorTypesPerNode [][]vactor.ActorType) []*dvactor.SystemConfig {
	t.Helper()
	ports := vt.FreePorts(t, totalNodes)
	cfgs := make([]*dvactor.SystemConfig, totalNodes)
	for i := 0; i < totalNodes; i++ {
		cfgs[i] = &dvactor.SystemConfig{
			SystemId:   vactor.SystemId(i + 1),
			Host:       "127.0.0.1",
			Port:       uint16(ports[i]),
			ActorTypes: actorTypesPerNode[i],
		}
	}
	return cfgs
}

// NewNode 用给定的集群配置创建一个未启动的节点（含日志捕获）。
func NewNode(t *testing.T, localSystemId vactor.SystemId, cfgs []*dvactor.SystemConfig,
	connectTimeout time.Duration, register func(s dvactor.ClusterSystem), opts ...ClusterOpt) (*vt.TestSystem, dvactor.ClusterSystem) {
	t.Helper()
	clusterCfg := &dvactor.ClusterConfig{
		LocalSystemId:  localSystemId,
		ConnectTimeout: connectTimeout,
		SystemConfigs:  cfgs,
	}
	for _, opt := range opts {
		opt(clusterCfg)
	}
	logs := vt.NewLogBuffer()
	s := dvactor.NewSystem(clusterCfg, func(sc *vactor.SystemConfig) {
		sc.TickInterval = 10 * time.Millisecond
		sc.DefaultStopInterval = 0
		sc.LogFunc = logs.LogFunc
	})
	if register != nil {
		register(s)
	}
	return vt.WrapSystem(s, logs), s
}
