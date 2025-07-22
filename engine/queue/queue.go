package queue

import (
	queueDef "github.com/kofplayer/dvactor/engine/queue/def"
	queueImpRing "github.com/kofplayer/dvactor/engine/queue/imp/ring"
)

func NewQueue(buffLen int) queueDef.Queue {
	r := new(queueImpRing.QQueue)
	err := r.Init(buffLen)
	if err != nil {
		panic(err)
	}
	return r
}
