package socketNetConnect

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 说明：本文件覆盖 engine 层读循环的两条健壮性契约——回调 panic 被 contained、
// 以及"同时返回数据与错误"时数据不被丢弃。二者都是集群链路不能出现的失态。

// onData 回调 panic 必须被转成"断这条连接"，而不是击穿进程；并且必须走完失败路径
// （关发送队列 + 触发断线回调），否则会留下 fd 仍开着、绑定状态永不清理的半死连接。
func TestOnDataPanicIsContainedAndDisconnects(t *testing.T) {
	var callbackReached atomic.Bool
	var mu sync.Mutex
	disconnected := 0

	cli, srv := newPair(t, 0, 0, func(cli, srv *ConnSocket) {
		srv.SetOnData(func(data []byte) error {
			callbackReached.Store(true)
			panic("boom from business callback")
		})
		srv.SetOnDisconnect(func() {
			mu.Lock()
			disconnected++
			mu.Unlock()
		})
		cli.SetOnData(func([]byte) error { return nil })
		cli.SetOnDisconnect(func() {})
	})

	if err := cli.SendData([]byte("trigger")); err != nil {
		t.Fatalf("SendData: %v", err)
	}
	waitFor(t, 3*time.Second, "srv disconnect callback after panicking onData", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return disconnected > 0
	})
	if !callbackReached.Load() {
		t.Fatal("onData was never invoked; the test did not exercise the panic path")
	}
	// 发送队列已被关闭：后续写入必须报错，而不是无声堆积
	if err := srv.SendData([]byte("after")); err == nil {
		t.Fatal("connection must be marked closed after a contained panic")
	}
}

// safeOnData 把 panic 变成 error（engine 层没有日志设施，堆栈由上层打印），
// 且不得吞掉正常路径的返回值。
func TestSafeOnDataConvertsPanicToError(t *testing.T) {
	c1, c2 := net.Pipe()
	defer func() { _ = c1.Close(); _ = c2.Close() }()
	c := newConnWithHeartbeat(c1, 0, 0)

	c.SetOnData(func([]byte) error { panic(int64(42)) })
	err := c.safeOnData([]byte("x"))
	if err == nil {
		t.Fatal("panic must be converted to an error")
	}
	if err.Error() != "onData callback panic: 42" {
		t.Fatalf("error must carry the panic value, got %q", err.Error())
	}

	c.SetOnData(func([]byte) error { return io.ErrUnexpectedEOF })
	if err := c.safeOnData([]byte("y")); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("wrapped error lost: %v", err)
	}
}

// scriptedConn 按预置脚本返回 (n, err)，用于构造"同一次 Read 既有数据又有错误"
// 这一无法用 net.Pipe 稳定复现的时序（TCP/带截止时间的读才会给出这种组合）。
type scriptedConn struct {
	net.Conn
	chunks []scriptedChunk
	idx    int
}

type scriptedChunk struct {
	data []byte
	err  error
}

func (c *scriptedConn) Read(p []byte) (int, error) {
	if c.idx >= len(c.chunks) {
		return 0, io.EOF
	}
	ch := c.chunks[c.idx]
	c.idx++
	n := copy(p, ch.data)
	return n, ch.err
}

func (c *scriptedConn) Close() error { return nil }

// Read 同时返回 n>0 与 err 时，那 n 个字节必须先交给回调——否则对端"发完即关"
// 场景下最后一个完整帧会静默消失（actor 少收一条消息，且重连也不会补发）。
func TestReceiverConsumesDataReturnedWithError(t *testing.T) {
	payload := []byte("last-frame-before-close")
	conn := &scriptedConn{chunks: []scriptedChunk{
		{data: payload, err: io.EOF}, // 数据与 EOF 同时到达
	}}
	cli := newConnWithHeartbeat(conn, 0, 0)
	cli.SetOnDisconnect(func() {})
	cli.SetOnData(func(b []byte) error { return nil })

	var mu sync.Mutex
	var received []byte
	// 用 onData 采集：回调收到的字节必须包含 payload
	cli.SetOnData(func(b []byte) error {
		mu.Lock()
		received = append(received, b...)
		mu.Unlock()
		return nil
	})

	done := make(chan struct{})
	go func() { cli.receiverRun(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("receiverRun did not return")
	}

	mu.Lock()
	got := string(received)
	mu.Unlock()
	if got != string(payload) {
		t.Fatalf("delivered %q, want %q", got, payload)
	}
}
