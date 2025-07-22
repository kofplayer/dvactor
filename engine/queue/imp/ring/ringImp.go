package queueImpRing

import "errors"

type QQueue struct {
	queue *Queue[interface{}]
}

// Init(baseBufferCount int) error
// Close() error
// IsClose() bool
// Enqueue(data interface{}) error
// Dequeue() (interface{}, bool)

func (q *QQueue) Init(baseBufferCount int) error {
	q.queue = NewQueue[interface{}](baseBufferCount)
	return nil
}

func (q *QQueue) Close() error {
	q.queue.Close()
	return nil
}

func (q *QQueue) IsClose() bool {
	return q.queue.IsClosed()
}

func (q *QQueue) Enqueue(data interface{}) error {
	if q.queue.Send(data) {
		return nil
	}
	return errors.New("closed")
}

func (q *QQueue) Dequeue() (interface{}, bool) {
	return q.queue.Receive()
}
