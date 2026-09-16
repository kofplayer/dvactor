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

// watchProxyRefresh 触发代理向 watchee 重新发起全部 watch 订阅。
// 仅在本机流转（重连成功后由 onSystemReconnected 投递），不跨节点序列化。
type watchProxyRefresh struct{}

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
		// 本代理不处理 MsgOnTick，关掉周期 tick 以免每秒被无谓唤醒。
		// 框架确需的投递不受影响：有待处理异步回调、或闲置回收条件满足时仍会收到 tick
		// （见 vactor 的 needTick），而 WatchProxy 恰好依赖后者在被闲置时回收自己。
		ctx.SetTickEnabled(false)
		wp.watcheeActorRef = GetWatcheeActorRef(ctx, ctx.GetActorRef().GetActorId())
	case *InnerWatch:
		wp.updateInnerWatch(ctx, e.WatchType, e.IsWatch, ctx.GetFromActorRef())
	case *OuterWatch:
		wp.updateOuterWatch(ctx, e.WatchType, e.IsWatch, e.Queue)
	case *vactor.MsgOnWatchMsg:
		if queues, ok := wp.queuess[e.WatchType]; ok {
			// 先收集再摘除：与 vactor 的 notify 保持同一写法，不依赖
			// "range 期间删除当前键是安全的"这一实现细节
			var stale []*vactor.Queue[interface{}]
			for queue := range queues {
				if !queue.Enqueue(&vactor.MsgOnWatchMsg{
					ActorRef:  wp.watcheeActorRef,
					WatchType: e.WatchType,
					Message:   e.Message,
				}) {
					stale = append(stale, queue)
				}
			}
			for _, queue := range stale {
				wp.updateOuterWatch(ctx, e.WatchType, false, queue)
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
			var stale []*vactor.Queue[interface{}]
			for queue := range queues {
				if !queue.Enqueue(e) {
					stale = append(stale, queue)
				}
			}
			for _, queue := range stale {
				wp.updateOuterWatch(ctx, watchType, false, queue)
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

// refreshWatches 对当前所有仍有订阅者的 WatchType 重新发起对 watchee 的 watch。
// 场景：分区期间发出的订阅（watch 信封发送失败）或对端节点重启导致
// watchee 侧订阅关系丢失；重连成功后由 onSystemReconnected 触发本方法自愈。
// 重复 watch 在 watchee 侧按 watcher 引用去重，幂等。
func (wp *WatchProxy) refreshWatches(ctx vactor.EnvelopeContext) {
	if wp.watcheeActorRef == nil {
		return
	}
	refreshed := make(map[vactor.WatchType]bool)
	for watchType := range wp.queuess {
		ctx.Watch(wp.watcheeActorRef, watchType)
		refreshed[watchType] = true
	}
	for watchType := range wp.watcherss {
		if !refreshed[watchType] {
			ctx.Watch(wp.watcheeActorRef, watchType)
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
	w, isImpl := watcher.(*vactor.ActorRefImpl)
	if !isImpl {
		ctx.LogError("watch proxy ignore unsupported ActorRef implementation %T", watcher)
		return
	}
	oldIsWatch := wp.isWatch(watchType)
	watchers, ok := wp.watcherss[watchType]
	if isWatch {
		if !ok {
			watchers = make(map[vactor.ActorRefImpl]bool)
			wp.watcherss[watchType] = watchers
		}
		if _, ok = watchers[*w]; !ok {
			watchers[*w] = true
		}
	} else {
		if ok {
			if _, ok := watchers[*w]; ok {
				delete(watchers, *w)
				if len(watchers) == 0 {
					delete(wp.watcherss, watchType)
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
