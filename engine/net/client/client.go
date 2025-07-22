package client

import (
	"encoding/binary"

	netConnect "github.com/kofplayer/dvactor/engine/net/connect"
)

func NewNetClient() NetClient {
	v := new(netClient)
	return v
}

type NetClient interface {
	SetConnector(connector netConnect.Connector)
	SetOnConnect(func())
	SetOnDisconnect(func())
	SetOnMessage(func(msgId uint32, data []byte) error)
	Connect() error
	Disconnect() error
	SendMessage(msgId uint32, data []byte) error
}

type netClient struct {
	connector    netConnect.Connector
	onConnect    func()
	onDisconnect func()
	onMessage    func(t uint32, data []byte) error
	receiveData  []byte
}

func (c *netClient) SetConnector(connector netConnect.Connector) {
	c.connector = connector
}

func (c *netClient) SetOnConnect(f func()) {
	c.onConnect = f
}

func (c *netClient) SetOnDisconnect(f func()) {
	c.onDisconnect = f
}

func (c *netClient) SetOnMessage(f func(msgId uint32, data []byte) error) {
	c.onMessage = f
}

func (c *netClient) Connect() error {
	c.connector.SetOnConnect(func() {
		if c.onConnect != nil {
			c.onConnect()
		}
	})
	c.connector.SetOnDisconnect(func() {
		if c.onDisconnect != nil {
			c.onDisconnect()
		}
	})
	c.connector.SetOnData(func(data []byte) error {
		// len(4) msgId(1) data
		if c.receiveData == nil {
			c.receiveData = data
		} else {
			c.receiveData = append(c.receiveData, data...)
		}
		for {
			l := uint32(len(c.receiveData))
			if l < 5 {
				return nil
			}
			dataLen := binary.BigEndian.Uint32(c.receiveData[0:4])
			msgLen := dataLen + 5
			if l < msgLen {
				return nil
			}
			t := uint32(c.receiveData[4])
			_data := c.receiveData[5:msgLen]
			c.onMessage(t, _data)
			c.receiveData = c.receiveData[msgLen:]
		}
	})
	return c.connector.Connect()
}

func (c *netClient) Disconnect() error {
	return c.connector.Disconnect()
}

func (c *netClient) SendMessage(msgId uint32, data []byte) error {
	l := len(data)
	_data := make([]byte, 5, l+5)
	binary.BigEndian.PutUint32(_data[:4], uint32(l))
	_data[4] = uint8(msgId)
	_data = append(_data, data...)
	return c.connector.SendData(_data)
}
