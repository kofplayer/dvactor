package netConnect

import (
	"encoding/binary"
	"fmt"
)

// 线协议帧格式: len(4,大端,仅data长度) + msgId(1) + data
const (
	PacketHeaderSize = 5
	MaxMsgId         = 0xFF
	// MaxPacketSize 单帧 data 长度上限。长度字段直接采信对端，必须设上限：
	// 否则恶意/损坏的字节流会先用超长长度把接收缓冲撑爆（内存耗尽），
	// 极端值（如 0xFFFFFFFF）还会使 len+5 溢出导致切片越界 panic。
	MaxPacketSize = 64 << 20
)

// PackMessage 打包一帧数据。msgId 超过 1 字节范围时返回错误（防止静默截断）。
func PackMessage(msgId uint32, data []byte) ([]byte, error) {
	if msgId > MaxMsgId {
		return nil, fmt.Errorf("msgId %v exceeds 1 byte range (max %v)", msgId, MaxMsgId)
	}
	l := len(data)
	pkt := make([]byte, PacketHeaderSize, PacketHeaderSize+l)
	binary.BigEndian.PutUint32(pkt[:4], uint32(l))
	pkt[4] = uint8(msgId)
	pkt = append(pkt, data...)
	return pkt, nil
}

// PacketSplitter 处理 TCP 粘包/半包，累积字节流并按帧切分。
type PacketSplitter struct {
	buf []byte
}

// Append 追加收到的字节（内部拷贝，调用方可安全复用读缓冲）。
func (p *PacketSplitter) Append(data []byte) {
	p.buf = append(p.buf, data...)
}

// Next 尝试取下一帧。
// ok=false 表示数据不足，需等待更多字节；
// err!=nil 表示长度字段非法（超过 MaxPacketSize），字节流已不可信，调用方必须断开连接。
func (p *PacketSplitter) Next() (msgId uint32, payload []byte, ok bool, err error) {
	l := uint32(len(p.buf))
	if l < PacketHeaderSize {
		return 0, nil, false, nil
	}
	dataLen := binary.BigEndian.Uint32(p.buf[0:4])
	if dataLen > MaxPacketSize {
		return 0, nil, false, fmt.Errorf("frame data length %d exceeds limit %d", dataLen, MaxPacketSize)
	}
	msgLen := dataLen + PacketHeaderSize
	if l < msgLen {
		return 0, nil, false, nil
	}
	msgId = uint32(p.buf[4])
	payload = p.buf[PacketHeaderSize:msgLen]
	p.buf = p.buf[msgLen:]
	if len(p.buf) == 0 {
		p.buf = nil // 释放底层数组，避免长连接内存驻留
	}
	return msgId, payload, true, nil
}
