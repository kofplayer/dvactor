package netConnect

type OnDisconnectFunc func()
type OnDataFunc func([]byte) error

type Conn interface {
	RemoteAddr() string
	Disconnect() error
	SendData([]byte) error
	SetOnDisconnect(OnDisconnectFunc)
	SetOnData(OnDataFunc)
}
