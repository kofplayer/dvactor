package netConnect

type OnConnectFunc func()

type Connector interface {
	Conn
	Connect() error
	SetOnConnect(OnConnectFunc)
}
