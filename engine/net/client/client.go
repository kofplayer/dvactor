package client

import (
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
	splitter     netConnect.PacketSplitter
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
		c.splitter.Append(data)
		for {
			msgId, payload, ok := c.splitter.Next()
			if !ok {
				return nil
			}
			c.onMessage(msgId, payload)
		}
	})
	return c.connector.Connect()
}

func (c *netClient) Disconnect() error {
	return c.connector.Disconnect()
}

func (c *netClient) SendMessage(msgId uint32, data []byte) error {
	pkt, err := netConnect.PackMessage(msgId, data)
	if err != nil {
		return err
	}
	return c.connector.SendData(pkt)
}
