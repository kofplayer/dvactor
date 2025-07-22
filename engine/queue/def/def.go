package queueDef

type Queue interface {
	Init(baseBufferCount int) error
	Close() error
	IsClose() bool
	Enqueue(data interface{}) error
	Dequeue() (interface{}, bool)
}
