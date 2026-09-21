package socketNetConnect

import (
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	netConnect "github.com/kofplayer/dvactor/engine/net/connect"
)

// 本文件覆盖断线与发送侧上界：二者共同决定"对端不再读数据"时本节点能否自愈。

// 关键回归：Disconnect 必须真的把 fd 关掉。
//
// 修复前 Disconnect 只做 q.Close()，把关 fd 交给 senderRun"排空队列之后"那一步；
// 对端不读时 senderRun 永久卡在 Write，于是 fd 不关、断线回调不触发、绑定状态
// 永不释放——对端随后还会被 "already registered" 拒绝重连，分区无法自愈。
// 这里让对端刻意不读，再 Disconnect，断言 fd 在有限时间内确实关闭。
func TestDisconnectClosesFdDespiteStuckWriter(t *testing.T) {
	cliConn, srvConn := net.Pipe()
	cli := newConnWithHeartbeat(cliConn, 0, 0)
	var mu sync.Mutex
	disconnected := 0
	cli.SetOnDisconnect(func() {
		mu.Lock()
		disconnected++
		mu.Unlock()
	})
	cli.SetOnData(func([]byte) error { return nil })
	go cli.receiverRun()
	go cli.senderRun()
	// srvConn 故意不读也不关：模拟"对端停止读取"

	// 先塞满 writer 的耐心：net.Pipe 无缓冲，第一帧就会让 senderRun 卡在 Write
	for i := 0; i < 8; i++ {
		if err := cli.SendData([]byte("pending-frame")); err != nil {
			t.Fatalf("SendData: %v", err)
		}
	}
	time.Sleep(50 * time.Millisecond) // 让 sender 确实进入 Write

	if err := cli.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}

	// 断线回调必须在有限时间内触发。修复前它永远不会触发：sender 卡在 Write 里到不了
	// 关 fd 那一步，receiver 又阻塞在没有读超时的 cliConn.Read 上（半开连接无人知晓）。
	fired := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		fired = disconnected > 0
		mu.Unlock()
		if fired {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !fired {
		_ = srvConn.Close()
		t.Fatal("disconnect callback never fired: half-dead connection cannot self-heal")
	}

	// 回调触发后 fd 已关：对端再也读不到东西。
	// （必须在回调之后才读——net.Pipe 会把等待中的写与新的读配对，提前读会替
	//  卡住的 senderRun 把数据"喂"出去，从而测不到原来的卡死路径。）
	_ = srvConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := srvConn.Read(make([]byte, 32))
	if err == nil {
		t.Fatalf("peer still readable after Disconnect (n=%d)", n)
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("peer read error = %v, want EOF/closed", err)
	}
	_ = srvConn.Close()
}

// 写超时是"卡住的 senderRun"的唯一解药：对端不收时 Write 会永久阻塞，
// 没有 deadline 就永远不会走到失败分支（关 fd + 回调）。
func TestWriteTimeoutBreaksStuckWriter(t *testing.T) {
	cliConn, srvConn := net.Pipe()
	cli := newConnWithHeartbeat(cliConn, 0, 0)
	cli.writeTimeout = 100 * time.Millisecond // 注入小值，避免测试等 30s
	var mu sync.Mutex
	fired := false
	cli.SetOnDisconnect(func() {
		mu.Lock()
		fired = true
		mu.Unlock()
	})
	cli.SetOnData(func([]byte) error { return nil })
	go cli.receiverRun()
	go cli.senderRun()

	if err := cli.SendData([]byte("will-never-fit")); err != nil {
		t.Fatalf("SendData: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := fired
		mu.Unlock()
		if got {
			_ = srvConn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = srvConn.Close()
	t.Fatal("write timeout did not break the stuck writer; no disconnect callback")
}

// 发送队列必须有上界：对端不收时，无界队列会把本端内存吃到 OOM，而 SendData
// 一路返回"成功"，上层据此误判已送达；心跳帧也走同一队列，链路还会一直"健康"。
func TestSendQueueIsBounded(t *testing.T) {
	// 生产默认值必须是有限的：新建连接的队列若没有上界，慢对端会一路堆到 OOM。
	fresh := newConnWithHeartbeat(nil, 0, 0)
	if got := fresh.q.MaxDepth(); got != netConnect.SendQueueMaxFrames {
		t.Fatalf("new connection queue depth cap = %d, want %d (unbounded queue is the bug)",
			got, netConnect.SendQueueMaxFrames)
	}

	cliConn, srvConn := net.Pipe()
	cli := newConnWithHeartbeat(cliConn, 0, 0)
	cli.q.SetMaxDepth(16) // 用实例内的小上限，避免改包级配置与其他连接竞争
	cli.SetOnData(func([]byte) error { return nil })
	cli.SetOnDisconnect(func() {})
	// 刻意不启动 senderRun：没有消费者，队列必然填满

	var fullErr error
	for i := 0; i < 200; i++ {
		if err := cli.SendData([]byte("frame")); err != nil {
			fullErr = err
			break
		}
	}
	if fullErr == nil {
		_ = srvConn.Close()
		t.Fatal("SendData accepted 200 frames into a 16-deep queue: the queue is unbounded")
	}
	if !errors.Is(fullErr, ErrSendQueueFull) {
		t.Fatalf("error = %v, want ErrSendQueueFull", fullErr)
	}
	if got := cli.q.Len(); got > 17 {
		t.Fatalf("queue depth = %d, must stay at or below the cap", got)
	}
	_ = srvConn.Close()
}

// 连接关闭后 SendData 必须报错（旧行为保留），且不能与"队满"混淆。
func TestSendDataAfterCloseReportsClosed(t *testing.T) {
	cliConn, srvConn := net.Pipe()
	cli := newConnWithHeartbeat(cliConn, 0, 0)
	cli.SetOnData(func([]byte) error { return nil })
	cli.SetOnDisconnect(func() {})
	_ = cli.Disconnect()
	if err := cli.SendData([]byte("x")); err == nil {
		t.Fatal("SendData on a closed connection must fail")
	}
	if err := cli.Disconnect(); err != nil {
		t.Fatalf("Disconnect must stay idempotent: %v", err)
	}
	_ = srvConn.Close()
}
