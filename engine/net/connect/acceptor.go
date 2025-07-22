package netConnect

type OnAcceptFunc func(Conn)

type Acceptor interface {
	Start() error
	Stop() error
	SetOnAccept(OnAcceptFunc)
}
