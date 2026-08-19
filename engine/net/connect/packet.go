package netConnect

import (
	"encoding/binary"
	"fmt"
)

// 线协议帧格式: len(4,大端,仅data长度) + msgId(1) + data
const (
	PacketHeaderSize = 5
	MaxMsgId         = 0xFF
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

// Next 尝试取下一帧。ok=false 表示数据不足，需等待更多字节。
func (p *PacketSplitter) Next() (msgId uint32, payload []byte, ok bool) {
	l := uint32(len(p.buf))
	if l < PacketHeaderSize {
		return 0, nil, false
	}
	dataLen := binary.BigEndian.Uint32(p.buf[0:4])
	msgLen := dataLen + PacketHeaderSize
	if l < msgLen {
		return 0, nil, false
	}
	msgId = uint32(p.buf[4])
	payload = p.buf[PacketHeaderSize:msgLen]
	p.buf = p.buf[msgLen:]
	if len(p.buf) == 0 {
		p.buf = nil // 释放底层数组，避免长连接内存驻留
	}
	return msgId, payload, true
}
