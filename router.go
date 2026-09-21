package dvactor

import (
	"strconv"
	"strings"

	"github.com/kofplayer/vactor"
)

// proxyActorId 生成跨节点代理的 ActorId："<被代理类型>-<被代理 id>"。
//
// 刻意手写拼接而不是 fmt.Sprintf：这在每次 watch / 外部请求的路径上，Sprintf 一次
// 要付掉反射与多个临时对象。与 GetWatcheeActorRef 的 SplitN 解析是一对，改动必须同步。
func proxyActorId(target vactor.ActorRef) vactor.ActorId {
	var buf []byte
	buf = strconv.AppendUint(buf, uint64(target.GetActorType()), 10)
	buf = append(buf, '-')
	buf = append(buf, target.GetActorId()...)
	return vactor.ActorId(buf)
}

func GetWatcheeActorRef(ctx vactor.EnvelopeContext, actorId vactor.ActorId) vactor.ActorRef {
	parts := strings.SplitN(string(actorId), "-", 2)
	if len(parts) != 2 {
		return nil
	}
	u64, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return nil
	}
	return ctx.CreateActorRef(vactor.ActorType(u64), vactor.ActorId(parts[1]))
}

func NewRouter(system vactor.System, clusterConfig *ClusterConfig, clusterNet *clusterNet) *Router {
	router := &Router{
		system:              system,
		systemId:            clusterConfig.LocalSystemId,
		actorType2SystemIds: make(map[vactor.ActorType][]vactor.SystemId),
		clusterNet:          clusterNet,
	}

	for _, config := range clusterConfig.SystemConfigs {
		for _, actorType := range config.ActorTypes {
			router.actorType2SystemIds[actorType] = append(router.actorType2SystemIds[actorType], config.SystemId)
		}
	}
	return router
}

type Router struct {
	system              vactor.System
	systemId            vactor.SystemId
	actorType2SystemIds map[vactor.ActorType][]vactor.SystemId
	clusterNet          *clusterNet
}

func (r *Router) CreateActorRefEx(systemId vactor.SystemId, actorType vactor.ActorType, actorId vactor.ActorId) vactor.ActorRef {
	ref := &vactor.ActorRefImpl{
		SystemId:  systemId,
		ActorType: actorType,
		ActorId:   actorId,
	}
	// 与 vactor 的单机分片共用同一个哈希函数，否则同一 actor 在单机分层
	// 与集群放置下会得到互不一致的落点。
	hash := vactor.HashActorId(actorId)
	systemIds := r.actorType2SystemIds[actorType]
	systemCount := uint32(len(systemIds))
	if ref.SystemId == 0 {
		if systemCount == 0 {
			r.system.LogError("actor type %v is not declared in any system config, fallback to local system %v", actorType, r.systemId)
			ref.SystemId = r.systemId
		} else {
			ref.SystemId = systemIds[hash%systemCount]
		}
	}
	if systemCount > 0 {
		ref.GroupSlot = vactor.GroupSlot((hash/systemCount)&0xFFFF + 1)
	} else {
		ref.GroupSlot = vactor.GroupSlot(hash&0xFFFF + 1)
	}
	if ref.GroupSlot == 0 {
		ref.GroupSlot = 1
	}
	return ref
}

// targetSystemId 取出信封目标所在的系统。目标缺失时返回 ok=false，调用方必须按
// "丢弃这一条"处理，绝不能直接解引用：Router 经常运行在业务 goroutine 或网络读
// goroutine 上（dvactor 的 clusterNet.OnMessage 就是后者），那里没有 recover，
// 一次 nil panic 就是整个进程崩溃。vactor 的 LocalRouter 有同款守卫，这里补齐。
func (r *Router) targetSystemId(envelope vactor.Envelope) (vactor.SystemId, bool) {
	to := envelope.GetToActorRef()
	if to == nil {
		r.system.LogError("router received envelope %T with nil target actor, dropped", envelope)
		return 0, false
	}
	return to.GetSystemId(), true
}

// splitBySystem 按目标系统给批量收件人分桶，同时滤掉 nil 引用。
//
// 两趟：先计数，再按精确容量分配。单趟 append 到 nil 切片时每个桶都要经历
// 1,2,4,...,N 的倍增——上千 watcher 的 notify 扇出正是这条路径（vactor 侧同款
// 改造实测 42→10 allocs）。
func (r *Router) splitBySystem(refs []vactor.ActorRef) map[vactor.SystemId][]vactor.ActorRef {
	counts := make(map[vactor.SystemId]int, len(r.clusterNet.systemInfos))
	skipped := 0
	for _, ref := range refs {
		if ref == nil {
			skipped++
			continue
		}
		counts[ref.GetSystemId()]++
	}
	if skipped > 0 {
		r.system.LogError("router dropped %d nil target actor ref(s) in batch envelope", skipped)
	}
	groups := make(map[vactor.SystemId][]vactor.ActorRef, len(counts))
	for systemId, n := range counts {
		groups[systemId] = make([]vactor.ActorRef, 0, n)
	}
	for _, ref := range refs {
		if ref == nil {
			continue
		}
		systemId := ref.GetSystemId()
		groups[systemId] = append(groups[systemId], ref)
	}
	return groups
}

func (r *Router) Router(envelope vactor.Envelope) vactor.VAError {
	var err vactor.VAError
	switch e := envelope.(type) {
	case *vactor.EnvelopeBatchSend:
		for systemId, actorRefs := range r.splitBySystem(e.ToActorRefs) {
			ebs := &vactor.EnvelopeBatchSend{
				FromActorRef: e.FromActorRef,
				ToActorRefs:  actorRefs,
				Messages:     e.Messages,
			}
			if systemId == r.systemId {
				_ = r.system.LocalRouter(ebs)
			} else if sendErr := r.clusterNet.Send(systemId, ebs); sendErr != nil {
				if err == nil {
					err = sendErr
				} else {
					r.system.LogError("router err: %v", sendErr)
				}
			}
		}
	case *vactor.EnvelopeWatch:
		if e.ToActorRef == nil {
			r.system.LogError("router received EnvelopeWatch with nil target actor, dropped")
			return vactor.NewVAError(vactor.ErrorCodeInvalidActor)
		}
		systemId := e.ToActorRef.GetSystemId()
		if systemId == r.systemId {
			_ = r.system.LocalRouter(e)
		} else {
			// FromActorRef 是订阅方；WatchProxy 的 watchee 引用解析失败时为 nil
			// （见 GetWatcheeActorRef 对畸形代理 id 的处理），此时无法归属订阅。
			if e.FromActorRef == nil {
				r.system.LogError("router received remote EnvelopeWatch without sender, dropped")
				return vactor.NewVAError(vactor.ErrorCodeInvalidActor)
			}
			if e.FromActorRef.GetActorType() == WatchProxyActorType {
				err = r.clusterNet.Send(systemId, e)
			} else {
				_ = r.system.LocalRouter(&vactor.EnvelopeSend{
					FromActorRef: e.FromActorRef,
					Message: &InnerWatch{
						WatchType: e.WatchType,
						IsWatch:   e.IsWatch,
					},
					ToActorRef: GetWatchProxyActorRef(r.system, r.systemId, e.ToActorRef),
				})
			}
		}
	case *vactor.EnvelopeNotify:
		// 与 clusterNet.Send 的对称防护：e.Message 为 nil 时下面的字段提取会 panic
		if e.Message == nil {
			r.system.LogError("router: envelope notify without payload, dropped")
			return vactor.NewVAError(ErrorCodeMessageLenError)
		}
		for systemId, actorRefs := range r.splitBySystem(e.ToActorRefs) {
			en := &vactor.EnvelopeNotify{
				FromActorRef: e.FromActorRef,
				ToActorRefs:  actorRefs,
				NotifyType:   e.NotifyType,
				Message: &vactor.MsgOnWatchMsg{
					ActorRef:  e.Message.ActorRef,
					WatchType: e.Message.WatchType,
					Message:   e.Message.Message,
				},
			}
			if systemId == r.systemId {
				_ = r.system.LocalRouter(en)
			} else if sendErr := r.clusterNet.Send(systemId, en); sendErr != nil {
				if err == nil {
					err = sendErr
				} else {
					r.system.LogError("router err: %v", sendErr)
				}
			}
		}
	case *vactor.EnvelopeOuterWatch:
		systemId, ok := r.targetSystemId(e)
		if !ok {
			return vactor.NewVAError(vactor.ErrorCodeInvalidActor)
		}
		if systemId == r.systemId {
			_ = r.system.LocalRouter(e)
		} else {
			_ = r.system.LocalRouter(&vactor.EnvelopeSend{
				Message: &OuterWatch{
					WatchType: e.WatchType,
					IsWatch:   e.IsWatch,
					Queue:     e.Queue,
				},
				ToActorRef: GetWatchProxyActorRef(r.system, r.systemId, e.ToActorRef),
			})
		}

	case *vactor.EnvelopeOuterRequest:
		systemId, ok := r.targetSystemId(e)
		if !ok {
			return vactor.NewVAError(vactor.ErrorCodeInvalidActor)
		}
		if systemId == r.systemId {
			_ = r.system.LocalRouter(e)
		} else {
			_ = r.system.LocalRouter(&vactor.EnvelopeSend{
				Message: &OuterRequest{
					ToActorRef: e.ToActorRef,
					Message:    e.Message,
					RspChan:    e.RspChan,
					Timeout:    e.Timeout,
				},
				ToActorRef: GetRequestProxyActorRef(r.system, r.systemId, e.ToActorRef),
			})
		}
	default:
		systemId, ok := r.targetSystemId(e)
		if !ok {
			return vactor.NewVAError(vactor.ErrorCodeInvalidActor)
		}
		if systemId == r.systemId {
			_ = r.system.LocalRouter(e)
		} else {
			err = r.clusterNet.Send(systemId, e)
		}
	}
	if err != nil {
		r.system.LogError("router err: %v\n", err)
	}
	return err
}
