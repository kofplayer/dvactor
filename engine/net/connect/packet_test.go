package netConnect_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	netConnect "github.com/kofplayer/dvactor/engine/net/connect"
)

func TestPackMessage(t *testing.T) {
	t.Parallel()

	t.Run("FrameLayout", func(t *testing.T) {
		pkt, err := netConnect.PackMessage(7, []byte("hello"))
		if err != nil {
			t.Fatal(err)
		}
		if len(pkt) != 5+5 {
			t.Fatalf("frame length = %d", len(pkt))
		}
		if l := binary.BigEndian.Uint32(pkt[0:4]); l != 5 {
			t.Fatalf("len field = %d", l)
		}
		if pkt[4] != 7 {
			t.Fatalf("msgId field = %d", pkt[4])
		}
		if !bytes.Equal(pkt[5:], []byte("hello")) {
			t.Fatalf("payload = %v", pkt[5:])
		}
	})

	t.Run("MsgIdRange", func(t *testing.T) {
		tests := []struct {
			name    string
			msgId   uint32
			wantErr bool
		}{
			{"max-1-byte-255", 255, false},
			{"over-1-byte-256", 256, true},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				_, err := netConnect.PackMessage(tt.msgId, nil)
				if (err != nil) != tt.wantErr {
					t.Fatalf("msgId %d: err = %v, wantErr %v", tt.msgId, err, tt.wantErr)
				}
			})
		}
	})
}

// TestPacketSplitter 用不同切块模式喂入同一条字节流（整段/半包/逐字节/任意分块/粘包），
// 验证拆包结果一致。
func TestPacketSplitter(t *testing.T) {
	t.Parallel()

	p1, err := netConnect.PackMessage(1, []byte("aaa"))
	if err != nil {
		t.Fatal(err)
	}
	p2, err := netConnect.PackMessage(2, []byte("bbbbb"))
	if err != nil {
		t.Fatal(err)
	}
	stream := append(append([]byte{}, p1...), p2...)

	// wantFirst = 首块喂入后应立即出帧的帧数
	// （half-packet/sticky/byte-by-byte 首块不足一个完整帧，必须等更多字节）
	patterns := []struct {
		name      string
		wantFirst int
		chunk     func(data []byte) [][]byte // 把字节流切成若干次 Append
	}{
		{"whole-stream", 2, func(data []byte) [][]byte { return [][]byte{data} }},
		{"half-packet", 0, func(data []byte) [][]byte {
			return [][]byte{data[:3], data[3:]}
		}},
		{"byte-by-byte", 0, func(data []byte) [][]byte {
			var chunks [][]byte
			for i := range data {
				chunks = append(chunks, data[i:i+1])
			}
			return chunks
		}},
		{"fixed-chunks", 0, func(data []byte) [][]byte {
			var chunks [][]byte
			for len(data) > 0 {
				n := 5
				if n > len(data) {
					n = len(data)
				}
				chunks = append(chunks, data[:n])
				data = data[n:]
			}
			return chunks
		}},
		{"sticky", 0, func(data []byte) [][]byte {
			// 第一帧头 + 两帧剩余一次给齐
			return [][]byte{data[:2], data[2:]}
		}},
	}
	for _, tt := range patterns {
		t.Run(tt.name, func(t *testing.T) {
			type frame struct {
				id      uint32
				payload string
			}
			var sp netConnect.PacketSplitter
			var got []frame
			drain := func() {
				for {
					id, payload, ok, err := sp.Next()
					if err != nil {
						t.Fatalf("unexpected splitter error: %v", err)
					}
					if !ok {
						return
					}
					got = append(got, frame{id, string(payload)})
				}
			}

			chunks := tt.chunk(stream)
			sp.Append(chunks[0])
			drain()
			if len(got) != tt.wantFirst {
				t.Fatalf("after first chunk got %d frames, want %d", len(got), tt.wantFirst)
			}
			for _, chunk := range chunks[1:] {
				sp.Append(chunk)
				drain()
			}
			want := []frame{{1, "aaa"}, {2, "bbbbb"}}
			if len(got) != len(want) {
				t.Fatalf("extracted %d frames, want %d: %+v", len(got), len(want), got)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("frame %d = %+v, want %+v", i, got[i], want[i])
				}
			}
		})
	}

	t.Run("EmptyPayload", func(t *testing.T) {
		pkt, err := netConnect.PackMessage(9, []byte{})
		if err != nil {
			t.Fatal(err)
		}
		if len(pkt) != 5 {
			t.Fatalf("empty payload frame length = %d", len(pkt))
		}
		var sp netConnect.PacketSplitter
		sp.Append(pkt)
		id, payload, ok, err := sp.Next()
		if err != nil || !ok || id != 9 || len(payload) != 0 {
			t.Fatalf("empty payload frame: id=%d len=%d ok=%v err=%v", id, len(payload), ok, err)
		}
	})

	t.Run("LargePayload", func(t *testing.T) {
		payload := bytes.Repeat([]byte{0xAB}, 1<<20) // 1MB
		pkt, err := netConnect.PackMessage(3, payload)
		if err != nil {
			t.Fatal(err)
		}
		var sp netConnect.PacketSplitter
		for len(pkt) > 0 {
			n := 7777
			if n > len(pkt) {
				n = len(pkt)
			}
			sp.Append(pkt[:n])
			pkt = pkt[n:]
			if len(pkt) > 0 {
				if _, _, ok, err := sp.Next(); err != nil || ok {
					t.Fatalf("frame should only be complete at the end, ok=%v err=%v", ok, err)
				}
			}
		}
		id, got, ok, err := sp.Next()
		if err != nil || !ok || id != 3 || !bytes.Equal(got, payload) {
			t.Fatalf("large payload mismatch: id=%d ok=%v len=%d err=%v", id, ok, len(got), err)
		}
	})
}

// TestSplitterRejectsOversizedFrame 长度字段超过 MaxPacketSize：Next 必须返回
// 错误而不是无限等待或 panic（含 uint32 溢出极端值）。
func TestSplitterRejectsOversizedFrame(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		dataLen uint32
	}{
		{"max-plus-one", netConnect.MaxPacketSize + 1},
		{"uint32-overflow-attempt", 0xFFFFFFFF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sp netConnect.PacketSplitter
			header := make([]byte, netConnect.PacketHeaderSize)
			binary.BigEndian.PutUint32(header[0:4], tt.dataLen)
			sp.Append(header)
			if _, _, _, err := sp.Next(); err == nil {
				t.Fatalf("dataLen %d should return error", tt.dataLen)
			}
		})
	}
}
