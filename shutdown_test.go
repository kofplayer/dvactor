package dvactor

import (
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/kofplayer/vactor"
	vt "github.com/kofplayer/vactor/testutil"
)

// ConnectTimeout 的取值解析：0 必须落到有限的默认值，负值才是无限等待。
// 默认无限等待是本轮检查里真实踩到的坑——Host 写错一个字符，Start() 就永久阻塞，
// 既不报错也不返回。
func TestConnectWaitTimeoutResolution(t *testing.T) {
	twoNodes := []*SystemConfig{{SystemId: 1, Port: 19951}, {SystemId: 2, Host: "127.0.0.1", Port: 19952}}
	cases := []struct {
		name    string
		cfgs    []*SystemConfig
		timeout time.Duration
		want    time.Duration // <0 = 无限等待
	}{
		{"unset with peers falls back to default", twoNodes, 0, DefaultConnectTimeout},
		{"explicit value is honored", twoNodes, 3 * time.Second, 3 * time.Second},
		{"negative keeps the legacy infinite wait", twoNodes, -1, -1},
		{"single node needs no mesh", []*SystemConfig{{SystemId: 1}}, 0, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewSystem(&ClusterConfig{
				LocalSystemId:  1,
				SystemConfigs:  c.cfgs,
				ConnectTimeout: c.timeout,
			}, func(sc *vactor.SystemConfig) {
				sc.LogFunc = func(vactor.LogLevel, string, ...interface{}) {}
			}).(*system)
			if got := s.clusterNet.connectWaitTimeout(); got != c.want {
				t.Fatalf("connectWaitTimeout = %v, want %v", got, c.want)
			}
		})
	}
}

// dialsPeerConfig 构造"本节点必须主动去连一个对端"的两节点配置。
//
// 组网方向由 SystemConfigs 的**下标**决定（下标大的去连下标小的，见 topology.go）：
// 本节点放在下标 1，才会产生一个真正的 clusterClient。把它放在下标 0 会得到一个
// 纯监听节点——那样的配置里根本没有客户端可测，测试会以错误的原因通过。
func dialsPeerConfig(localPort, peerPort uint16, connectTimeout time.Duration) *ClusterConfig {
	return &ClusterConfig{
		LocalSystemId:  1,
		ConnectTimeout: connectTimeout,
		SystemConfigs: []*SystemConfig{
			{SystemId: 2, Host: "127.0.0.1", Port: peerPort},
			{SystemId: 1, Host: "127.0.0.1", Port: localPort},
		},
	}
}

// Start 在等不到互连时必须带错误返回，而不是永久卡住。
func TestStartReturnsErrorWhenPeerNeverRegisters(t *testing.T) {
	ports := vt.FreePorts(t, 1)
	s := NewSystem(dialsPeerConfig(uint16(ports[0]), uint16(ports[0]+1), 2*time.Second),
		func(sc *vactor.SystemConfig) {
			sc.LogFunc = func(vactor.LogLevel, string, ...interface{}) {}
		})
	startDone := make(chan struct{})
	go func() {
		defer close(startDone)
		s.Start()
	}()
	select {
	case <-startDone:
	case <-time.After(15 * time.Second):
		t.Fatal("Start() did not return: the connect wait has no effective deadline")
	}
	if s.ClusterStartError() == nil {
		t.Fatal("ClusterStartError must report the incomplete mesh")
	}
	s.Stop()
}

// 停机必须真的把已建立的集群连接关掉。
//
// 修复前 clusterClient.Stop() 只 close(c.closed)：循环从"等注册响应"的 select 里
// 退出时连接仍然 OPEN，Stop() 返回后 TCP 还挂着、泄漏的读 goroutine 继续回 Pong，
// 于是对端把这条链路当作健康并持续向一个已停机的进程投递。
func TestStopClosesEstablishedClusterConnection(t *testing.T) {
	// 一个只 accept、永不注册的假对端：让 client 停在"等注册响应"这一步
	ports := vt.FreePorts(t, 1)
	peerPort := uint16(ports[0] + 1)
	fake, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", peerPort))
	if err != nil {
		t.Fatalf("fake peer listen: %v", err)
	}
	defer func() { _ = fake.Close() }()
	accepted := make(chan net.Conn, 16)
	go func() {
		for {
			conn, err := fake.Accept()
			if err != nil {
				return
			}
			accepted <- conn
		}
	}()

	s := NewSystem(dialsPeerConfig(uint16(ports[0]), peerPort, 3*time.Second),
		func(sc *vactor.SystemConfig) {
			sc.LogFunc = func(vactor.LogLevel, string, ...interface{}) {}
		})
	s.Start()

	var peer net.Conn
	select {
	case peer = <-accepted:
	case <-time.After(8 * time.Second):
		s.Stop()
		t.Fatal("client never connected to the fake peer; the test proved nothing")
	}
	// 读光对端数据：只有本端真的关掉 fd，这里才会拿到 EOF/错误
	_ = peer.SetReadDeadline(time.Now().Add(8 * time.Second))

	s.Stop()

	// 排掉 Stop 之前重连循环建立的其它连接
	for {
		select {
		case extra := <-accepted:
			_ = extra.Close()
			continue
		default:
		}
		break
	}

	// 读到"连接已断"为止。注意 Stop 之前对端可能已经收到注册请求：那些字节要先读完，
	// 只有最终的**非超时**错误才证明是本端关掉了 fd（超时说明链路还开着）。
	buf := make([]byte, 64)
	for {
		n, err := peer.Read(buf)
		if err == nil {
			continue
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatalf("peer connection still open after Stop (last read delivered %d bytes, then timed out)", n)
		}
		break // EOF / connection reset / closed：均说明本端 fd 已关
	}
	_ = peer.Close()
}

// 重连循环在停机时必须退出，且退出后不再新建连接。
func TestStopTerminatesReconnectLoop(t *testing.T) {
	ports := vt.FreePorts(t, 1)
	peerPort := uint16(ports[0] + 1) // 没人监听：client 会按退避不停重试
	s := NewSystem(dialsPeerConfig(uint16(ports[0]), peerPort, 1*time.Second),
		func(sc *vactor.SystemConfig) {
			sc.LogFunc = func(vactor.LogLevel, string, ...interface{}) {}
		})
	s.Start()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Stop()
	}()
	select {
	case <-done:
	case <-time.After(clientStopTimeout + 5*time.Second):
		t.Fatal("Stop() did not return: the reconnect loop is not terminable")
	}

	// 停机后不应再有新的集群活动：占住那个空端口，确认没人在重试窗口内来连
	probe, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", peerPort))
	if err != nil {
		t.Skipf("cannot probe port %d: %v", peerPort, err)
	}
	defer func() { _ = probe.Close() }()
	dialed := make(chan struct{})
	go func() {
		if conn, err := probe.Accept(); err == nil {
			_ = conn.Close()
			close(dialed)
		}
	}()
	select {
	case <-dialed:
		t.Fatal("a cluster client kept dialing after Stop()")
	case <-time.After(2 * time.Second):
		// 整个重连退避窗口内没有任何连接尝试：循环确实退出了
	}
}

// 重连退避必须从远小于 ConnectTimeout 的值起指数增长。
// 固定 5s 的退避与常见互连超时同量级：对端只晚起几毫秒，也会让人第一次失败后
// 直接睡满 5s、把整个等待窗口吃光，表现为"集群起不来"。
func TestNextRetryDelayGrowsAndCaps(t *testing.T) {
	// 首轮必须远小于常见互连超时，否则一次竞态就吃掉整个窗口
	seq := []time.Duration{
		0, reconnectBackoffMin, reconnectBackoffMin * 2, reconnectBackoffMin * 4,
	}
	for _, cur := range seq {
		got := nextRetryDelay(cur)
		if got > reconnectBackoffMax {
			t.Fatalf("nextRetryDelay(%v) = %v, above the cap", cur, got)
		}
		if got <= cur && cur > 0 {
			t.Fatalf("nextRetryDelay(%v) = %v, must grow", cur, got)
		}
	}
	if got := nextRetryDelay(0); got >= reconnectBackoffMax {
		t.Fatalf("first retry waits %v, must be far below the cap", got)
	}
	// 封顶后不再增长
	if got := nextRetryDelay(reconnectBackoffMax); got != reconnectBackoffMax {
		t.Fatalf("nextRetryDelay(max) = %v, want %v", got, reconnectBackoffMax)
	}
}

// 停机信号必须打断当前退避等待，否则 Stop 会被一轮睡眠拖住。
func TestClusterClientBackoffInterruptible(t *testing.T) {
	c := NewClusterClient(nil, 1)
	close(c.closed)
	start := time.Now()
	if c.backoff() {
		t.Fatal("backoff must return false once the client is closed")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("backoff waited %v after close", elapsed)
	}
	if c.retryDelay != reconnectBackoffMin {
		t.Fatalf("retryDelay = %v, want %v", c.retryDelay, reconnectBackoffMin)
	}
	c.noteConnected()
	if c.retryDelay != 0 {
		t.Fatalf("retryDelay = %v after noteConnected, want 0", c.retryDelay)
	}
}
