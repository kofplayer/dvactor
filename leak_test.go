package dvactor_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/kofplayer/dvactor"
	dtu "github.com/kofplayer/dvactor/testutil"
	"github.com/kofplayer/vactor"
)

// 集群停机必须把网络侧 goroutine 全部放掉：重连循环、每条连接的 receiver/sender/heartbeat。
//
// 本用例只守"goroutine 数不随启停轮数增长"。它**抓不到**停机时漏掉 Disconnect：那种情况
// 下 goroutine 会因为对端已关而照样退出，只是 socket 一直 OPEN——那一行由
// TestStopClosesEstablishedClusterConnection 守（两者分工不同，都已做过回退验证）。
func TestClusterStopReleasesGoroutines(t *testing.T) {
	settledCount := func() int {
		var last, same int
		for i := 0; i < 60; i++ {
			time.Sleep(50 * time.Millisecond)
			n := runtime.NumGoroutine()
			if n == last {
				if same++; same >= 3 {
					return n
				}
			} else {
				same = 0
			}
			last = n
		}
		return last
	}

	cycle := func(label string) {
		cfgs := dtu.BuildClusterConfig(t, 2, [][]vactor.ActorType{
			{clusterEchoType}, {clusterEchoType},
		})
		// 两个节点必须并发 Start：拓扑里下标小的监听、大的外连，顺序启动会让监听方
		// 白等一整个 ConnectTimeout
		nodes := make([]dvactor.ClusterSystem, 2)
		started := make(chan struct{}, 2)
		for i := 0; i < 2; i++ {
			_, nodes[i] = dtu.NewNode(t, vactor.SystemId(i+1), cfgs, 10*time.Second,
				func(s dvactor.ClusterSystem) {
					s.RegisterMessageType(clusterStrMsgType, strValCreator)
					registerEcho(s)
				})
			go func(n dvactor.ClusterSystem) {
				n.Start()
				started <- struct{}{}
			}(nodes[i])
		}
		for i := 0; i < 2; i++ {
			select {
			case <-started:
			case <-time.After(12 * time.Second):
				t.Fatal("cluster did not form within 12s")
			}
		}
		n1 := nodes[0]
		if _, err := n1.Request(n1.CreateActorRef(clusterEchoType, "e"),
			newStrValMsg("ping"), 5*time.Second); err != nil {
			t.Fatalf("%s: cross-node request: %v", label, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, n := range nodes {
			if err := n.StopWithContext(ctx); err != nil {
				t.Fatalf("%s: StopWithContext: %v", label, err)
			}
		}
	}

	// 预热一轮再取基线（首轮会拉起惰性运行时 goroutine）
	cycle("warmup")
	base := settledCount()

	const rounds = 4
	for r := 0; r < rounds; r++ {
		cycle("cycle")
	}
	if got := settledCount(); got > base+8 {
		t.Fatalf("goroutines leaked across %d cluster start/stop cycles: baseline %d, now %d "+
			"(每轮至少应留下重连循环+连接收发协程)", rounds, base, got)
	}
}
