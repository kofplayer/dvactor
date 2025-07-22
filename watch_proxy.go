package dvactor

import (
	"fmt"
	"time"

	"github.com/kofplayer/vactor"
)

const WatchProxyActorType vactor.ActorType = vactor.ActorTypeStart + 2

func GetWatchProxyActorRef(system vactor.System, systemId vactor.SystemId, watcheeActorRef vactor.ActorRef) vactor.ActorRef {
	return system.CreateActorRefEx(systemId, WatchProxyActorType, vactor.ActorId(fmt.Sprintf("%v-%v", watcheeActorRef.GetActorType(), watcheeActorRef.GetActorId())))
}

func NewWatchProxy() *WatchProxy {
	return &WatchProxy{
		queuess:   make(map[vactor.WatchType]map[*vactor.Queue[interface{}]]bool),
		watcherss: make(map[vactor.WatchType]map[vactor.ActorRefImpl]bool),
	}
}

type InnerWatch struct {
	WatchType vactor.WatchType
	IsWatch   bool
}

type OuterWatch struct {
	WatchType vactor.WatchType
	IsWatch   bool
	Queue     *vactor.Queue[interface{}]
}

type WatchProxy struct {
	queuess         map[vactor.WatchType]map[*vactor.Queue[interface{}]]bool
	watcherss       map[vactor.WatchType]map[vactor.ActorRefImpl]bool
	watcheeActorRef vactor.ActorRef
}

func (wp *WatchProxy) OnMessage(ctx vactor.EnvelopeContext) {
	switch e := ctx.GetMessage().(type) {
	case *vactor.MsgOnStart:
		wp.watcheeActorRef = GetWatcheeActorRef(ctx, ctx.GetActorRef().GetActorId())
	case *InnerWatch:
		wp.updateInnerWatch(ctx, e.WatchType, e.IsWatch, ctx.GetFromActorRef())
	case *OuterWatch:
		wp.updateOuterWatch(ctx, e.WatchType, e.IsWatch, e.Queue)
	case *vactor.MsgOnWatchMsg:
		if queues, ok := wp.queuess[e.WatchType]; ok {
			for queue := range queues {
				if (!queue.Enqueue(&vactor.MsgOnWatchMsg{
					ActorRef:  wp.watcheeActorRef,
					WatchType: e.WatchType,
					Message:   e.Message,
				})) {
					wp.updateOuterWatch(ctx, e.WatchType, false, queue)
				}
			}
		}
		if watchers, ok := wp.watcherss[e.WatchType]; ok {
			actorRefs := make([]vactor.ActorRef, 0, len(watchers))
			for watcher := range watchers {
				actorRefs = append(actorRefs, &watcher)
			}
			ctx.LocalRouter(&vactor.EnvelopeBatchSend{
				FromActorRef: wp.watcheeActorRef,
				ToActorRefs:  actorRefs,
				Messages:     []interface{}{e},
			})
		}
	case *vactor.MsgOnEventMsg:
		watchType := vactor.WatchType(e.EventId)
		if queues, ok := wp.queuess[watchType]; ok {
			for queue := range queues {
				if !queue.Enqueue(e) {
					wp.updateOuterWatch(ctx, watchType, false, queue)
				}
			}
		}
		if watchers, ok := wp.watcherss[watchType]; ok {
			actorRefs := make([]vactor.ActorRef, 0, len(watchers))
			for watcher := range watchers {
				actorRefs = append(actorRefs, &watcher)
			}
			ctx.LocalRouter(&vactor.EnvelopeBatchSend{
				FromActorRef: wp.watcheeActorRef,
				ToActorRefs:  actorRefs,
				Messages:     []interface{}{e},
			})
		}
	}
}

func (wp *WatchProxy) isWatch(watchType vactor.WatchType) bool {
	_, ok := wp.queuess[watchType]
	if ok {
		return true
	}
	_, ok = wp.watcherss[watchType]
	return ok
}

func (wp *WatchProxy) updateInnerWatch(ctx vactor.EnvelopeContext, watchType vactor.WatchType, isWatch bool, watcher vactor.ActorRef) {
	oldIsWatch := wp.isWatch(watchType)
	watchers, ok := wp.watcherss[watchType]
	if isWatch {
		if !ok {
			watchers = make(map[vactor.ActorRefImpl]bool)
			wp.watcherss[watchType] = watchers
		}
		w := watcher.(*vactor.ActorRefImpl)
		if _, ok = watchers[*w]; !ok {
			watchers[*w] = true
		}
	} else {
		if ok {
			w := watcher.(*vactor.ActorRefImpl)
			if _, ok := watchers[*w]; ok {
				delete(watchers, *w)
				if len(watchers) == 0 {
					delete(wp.queuess, watchType)
				}
			}
		}
	}
	newIsWatch := wp.isWatch(watchType)
	if !oldIsWatch && newIsWatch {
		ctx.SetStopInterval(0)
		ctx.Watch(wp.watcheeActorRef, watchType)
	} else if oldIsWatch && !newIsWatch {
		if len(wp.queuess) == 0 && len(wp.watcherss) == 0 {
			ctx.SetStopInterval(time.Second * 10)
		}
		ctx.Unwatch(wp.watcheeActorRef, watchType)
	}
}

func (wp *WatchProxy) updateOuterWatch(ctx vactor.EnvelopeContext, watchType vactor.WatchType, isWatch bool, queue *vactor.Queue[interface{}]) {
	oldIsWatch := wp.isWatch(watchType)
	queues, ok := wp.queuess[watchType]
	if isWatch {
		if !ok {
			queues = make(map[*vactor.Queue[interface{}]]bool)
			wp.queuess[watchType] = queues
		}
		if _, ok = queues[queue]; !ok {
			queues[queue] = true
		}
	} else {
		if ok {
			if _, ok := queues[queue]; ok {
				delete(queues, queue)
				if len(queues) == 0 {
					delete(wp.queuess, watchType)
				}
			}
		}
	}
	newIsWatch := wp.isWatch(watchType)
	if !oldIsWatch && newIsWatch {
		ctx.SetStopInterval(0)
		ctx.Watch(wp.watcheeActorRef, watchType)
	} else if oldIsWatch && !newIsWatch {
		if len(wp.queuess) == 0 && len(wp.watcherss) == 0 {
			ctx.SetStopInterval(time.Second * 10)
		}
		ctx.Unwatch(wp.watcheeActorRef, watchType)
	}
}
