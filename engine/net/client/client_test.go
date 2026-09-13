package client

import (
	"errors"
	"testing"

	netConnect "github.com/kofplayer/dvactor/engine/net/connect"
)

// fakeConnector 实现 netConnect.Connector，让 NetClient 的逻辑可以在无网络条件下驱动。
type fakeConnector struct {
	onConnect    netConnect.OnConnectFunc
	onDisconnect netConnect.OnDisconnectFunc
	onData       netConnect.OnDataFunc

	connectErr  error
	sendErr     error
	connected   bool
	disconnects int
	sent        [][]byte
}

func (f *fakeConnector) RemoteAddr() string { return "fake:0" }

func (f *fakeConnector) Disconnect() error {
	f.disconnects++
	f.connected = false
	return nil
}

func (f *fakeConnector) SendData(data []byte) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, append([]byte(nil), data...))
	return nil
}

func (f *fakeConnector) SetOnDisconnect(cb netConnect.OnDisconnectFunc) { f.onDisconnect = cb }
func (f *fakeConnector) SetOnData(cb netConnect.OnDataFunc)             { f.onData = cb }
func (f *fakeConnector) SetOnConnect(cb netConnect.OnConnectFunc)       { f.onConnect = cb }

func (f *fakeConnector) Connect() error {
	if f.connectErr != nil {
		return f.connectErr
	}
	f.connected = true
	if f.onConnect != nil {
		f.onConnect()
	}
	return nil
}

// 连接成功会触发 OnConnect；Connect/Messages/心跳/Disconnect 全链路可驱动。
func TestNetClientConnectSendAndHeartbeat(t *testing.T) {
	fc := &fakeConnector{}
	c := NewNetClient()
	c.SetConnector(fc)

	connected := 0
	c.SetOnConnect(func() { connected++ })
	disconnected := 0
	c.SetOnDisconnect(func() { disconnected++ })
	received := make([]byte, 0, 8)
	c.SetOnMessage(func(msgId uint32, data []byte) error {
		received = append(received, data...)
		return nil
	})

	if err := c.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if connected != 1 || !fc.connected {
		t.Fatalf("onConnect not fired (connected=%d)", connected)
	}

	// 业务帧：msgId=7 + payload
	want, err := netConnect.PackMessage(7, []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SendMessage(7, []byte("payload")); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(fc.sent) != 1 {
		t.Fatalf("expected 1 frame sent, got %d", len(fc.sent))
	}
	got := fc.sent[0]
	if len(got) != len(want) {
		t.Fatalf("frame length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("frame mismatch at %d: %v vs %v", i, got, want)
		}
	}

	// 心跳 Ping 必须被拦截并自动回 Pong，不进入业务层
	fc.sent = nil
	ping, err := netConnect.PackMessage(netConnect.HeartbeatMsgIdPing, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := fc.onData(ping); err != nil {
		t.Fatalf("ping handling: %v", err)
	}
	if len(fc.sent) != 1 {
		t.Fatalf("expected auto pong, sent %d frames", len(fc.sent))
	}
	pong, _ := netConnect.PackMessage(netConnect.HeartbeatMsgIdPong, nil)
	if len(fc.sent[0]) != len(pong) || fc.sent[0][4] != pong[4] {
		t.Fatalf("expected pong frame, got %v", fc.sent[0])
	}
	if len(received) != 0 {
		t.Fatalf("heartbeat must not reach business layer: %v", received)
	}

	// Pong 直接忽略
	fc.sent = nil
	if err := fc.onData(pong); err != nil {
		t.Fatalf("pong handling: %v", err)
	}
	if len(fc.sent) != 0 || len(received) != 0 {
		t.Fatalf("pong should be ignored")
	}

	// 业务帧分发到 OnMessage
	biz, _ := netConnect.PackMessage(9, []byte("abc"))
	if err := fc.onData(biz); err != nil {
		t.Fatalf("business frame: %v", err)
	}
	if string(received) != "abc" {
		t.Fatalf("received = %q, want abc", received)
	}

	if err := c.Disconnect(); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if fc.disconnects != 1 {
		t.Fatalf("disconnects = %d, want 1", fc.disconnects)
	}
	if disconnected != 0 {
		t.Fatal("Disconnect() itself must not fire the onDisconnect callback")
	}
}

// 未注册 OnMessage 时业务帧被静默忽略（不 panic）。
func TestNetClientWithoutOnMessage(t *testing.T) {
	fc := &fakeConnector{}
	c := NewNetClient()
	c.SetConnector(fc)
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	frame, _ := netConnect.PackMessage(3, []byte("x"))
	if err := fc.onData(frame); err != nil {
		t.Fatalf("frame without handler should be ignored, got %v", err)
	}
}

// 业务回调返回错误必须上抛，触发上层断线。
func TestNetClientHandlerErrorPropagates(t *testing.T) {
	fc := &fakeConnector{}
	c := NewNetClient()
	c.SetConnector(fc)
	boom := errors.New("handler boom")
	c.SetOnMessage(func(msgId uint32, data []byte) error { return boom })
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	frame, _ := netConnect.PackMessage(1, []byte("x"))
	if err := fc.onData(frame); !errors.Is(err, boom) {
		t.Fatalf("expected handler error, got %v", err)
	}
}

// 非法帧长（超过上限）必须报错，提示调用方断线。
func TestNetClientRejectsOversizedFrame(t *testing.T) {
	fc := &fakeConnector{}
	c := NewNetClient()
	c.SetConnector(fc)
	c.SetOnMessage(func(uint32, []byte) error { return nil })
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	// 手工构造长度字段超过 MaxPacketSize 的帧头
	bad := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0x01}
	if err := fc.onData(bad); err == nil {
		t.Fatal("oversized frame length should return an error")
	}
}

// 半包：数据不足时不得触发业务回调，补齐后正常分发。
func TestNetClientHandlesPartialFrame(t *testing.T) {
	fc := &fakeConnector{}
	c := NewNetClient()
	c.SetConnector(fc)
	var got []byte
	c.SetOnMessage(func(msgId uint32, data []byte) error {
		got = append(got, data...)
		return nil
	})
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	frame, _ := netConnect.PackMessage(2, []byte("hello"))
	if err := fc.onData(frame[:3]); err != nil {
		t.Fatalf("partial frame: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("partial frame must not dispatch: %v", got)
	}
	if err := fc.onData(frame[3:]); err != nil {
		t.Fatalf("completing frame: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q, want hello", got)
	}
}

// 错误路径：Connect / SendMessage 的错误必须原样上抛。
func TestNetClientErrorPaths(t *testing.T) {
	connErr := errors.New("dial failed")
	fc := &fakeConnector{connectErr: connErr}
	c := NewNetClient()
	c.SetConnector(fc)
	if err := c.Connect(); !errors.Is(err, connErr) {
		t.Fatalf("expected connect error, got %v", err)
	}

	fc2 := &fakeConnector{sendErr: errors.New("write failed")}
	c2 := NewNetClient()
	c2.SetConnector(fc2)
	if err := c2.Connect(); err != nil {
		t.Fatal(err)
	}
	if err := c2.SendMessage(1, []byte("x")); err == nil {
		t.Fatal("expected send error to propagate")
	}
	// msgId 超过 1 字节范围：打包阶段就应报错，不产生发送
	if err := c2.SendMessage(256, nil); err == nil {
		t.Fatal("msgId 256 should be rejected")
	}
}
