package queueImpRing

// RingBuffer 表示一个非线程安全的环形缓冲区
type RingBuffer[T any] struct {
	buffer []T // 用于存储数据的切片
	size   int // 缓冲区大小
	head   int // 头指针，指向下一个读取位置
	tail   int // 尾指针，指向下一个写入位置
	count  int // 当前缓冲区中的元素数量
	zero   T
}

// New 创建一个新的环形缓冲区
func NewRingBuffer[T any](size int) *RingBuffer[T] {
	if size <= 0 {
		panic("ring buffer size must be greater than 0")
	}
	return &RingBuffer[T]{
		buffer: make([]T, size),
		size:   size,
		head:   0,
		tail:   0,
		count:  0,
	}
}

// Push 向环形缓冲区添加一个元素
func (rb *RingBuffer[T]) Push(value T) {
	if rb.IsFull() {
		newSize := rb.size * 2
		newBuffer := make([]T, newSize)
		if rb.head < rb.tail {
			copy(newBuffer, rb.buffer[rb.head:rb.tail])
		} else {
			n := copy(newBuffer, rb.buffer[rb.head:rb.size])
			copy(newBuffer[n:], rb.buffer[0:rb.tail])
		}
		rb.buffer = newBuffer
		rb.size = newSize
		rb.head = 0
		rb.tail = rb.count
	}

	rb.buffer[rb.tail] = value
	rb.tail = (rb.tail + 1) % rb.size
	rb.count++
}

// PushBatch 批量向环形缓冲区添加元素
func (rb *RingBuffer[T]) PushBatch(values []T) {
	n := len(values)
	if n == 0 {
		return
	}
	// 预扩容，确保空间足够
	for rb.size-rb.count < n {
		newSize := rb.size * 2
		newBuffer := make([]T, newSize)
		if rb.head < rb.tail {
			copy(newBuffer, rb.buffer[rb.head:rb.tail])
		} else {
			c := copy(newBuffer, rb.buffer[rb.head:rb.size])
			copy(newBuffer[c:], rb.buffer[0:rb.tail])
		}
		rb.buffer = newBuffer
		rb.size = newSize
		rb.head = 0
		rb.tail = rb.count
	}
	// 批量写入
	for _, value := range values {
		rb.buffer[rb.tail] = value
		rb.tail = (rb.tail + 1) % rb.size
		rb.count++
	}
}

// Pop 从环形缓冲区取出一个元素
// 如果缓冲区为空，返回零值和false；否则返回元素值和true
func (rb *RingBuffer[T]) Pop() (T, bool) {
	if rb.IsEmpty() {
		return rb.zero, false
	}

	value := rb.buffer[rb.head]
	rb.buffer[rb.head] = rb.zero // 清除引用，帮助垃圾回收
	rb.head = (rb.head + 1) % rb.size
	rb.count--
	return value, true
}

// PopAll 弹出所有元素并清空缓冲区，返回按顺序的切片
// func (rb *RingBuffer[T]) PopAll() []T {
// 	if rb.IsEmpty() {
// 		return []T{}
// 	}
// 	result := make([]T, rb.count)
// 	if rb.head < rb.tail {
// 		// 数据在物理内存中是连续的
// 		copy(result, rb.buffer[rb.head:rb.tail])
// 	} else {
// 		// 数据被分成两段
// 		n := copy(result, rb.buffer[rb.head:rb.size])
// 		copy(result[n:], rb.buffer[0:rb.tail])
// 	}
// 	// 清除引用，帮助GC
// 	if rb.head < rb.tail {
// 		for i := rb.head; i < rb.tail; i++ {
// 			rb.buffer[i] = rb.zero
// 		}
// 	} else {
// 		for i := rb.head; i < rb.size; i++ {
// 			rb.buffer[i] = rb.zero
// 		}
// 		for i := 0; i < rb.tail; i++ {
// 			rb.buffer[i] = rb.zero
// 		}
// 	}
// 	rb.head = 0
// 	rb.tail = 0
// 	rb.count = 0
// 	return result
// }

// PopAll 弹出所有元素并清空缓冲区，返回按顺序的切片
func (rb *RingBuffer[T]) PopAll() []T {
	if rb.IsEmpty() {
		return []T{}
	}
	result := make([]T, rb.count)
	for i := 0; i < rb.count; i++ {
		index := (rb.head + i) % rb.size
		result[i] = rb.buffer[index]
		// 可选：清除引用，帮助GC
		rb.buffer[index] = rb.zero
	}
	rb.head = 0
	rb.tail = 0
	rb.count = 0
	return result
}

// Peek 查看环形缓冲区头部元素但不移除
// 如果缓冲区为空，返回零值和false；否则返回元素值和true
func (rb *RingBuffer[T]) Peek() (T, bool) {
	if rb.IsEmpty() {
		return rb.zero, false
	}

	return rb.buffer[rb.head], true
}

// IsEmpty 检查环形缓冲区是否为空
func (rb *RingBuffer[T]) IsEmpty() bool {
	return rb.count == 0
}

// IsFull 检查环形缓冲区是否已满
func (rb *RingBuffer[T]) IsFull() bool {
	return rb.count == rb.size
}

// Size 返回环形缓冲区的容量
func (rb *RingBuffer[T]) Size() int {
	return rb.size
}

// Count 返回环形缓冲区中的元素数量
func (rb *RingBuffer[T]) Count() int {
	return rb.count
}

// Clear 清空环形缓冲区
func (rb *RingBuffer[T]) Clear() {
	rb.head = 0
	rb.tail = 0
	rb.count = 0
}

// GetAll 获取环形缓冲区中的所有元素（按顺序）
func (rb *RingBuffer[T]) GetAll() []T {
	if rb.IsEmpty() {
		return []T{}
	}

	result := make([]T, rb.count)
	for i := 0; i < rb.count; i++ {
		index := (rb.head + i) % rb.size
		result[i] = rb.buffer[index]
	}

	return result
}
