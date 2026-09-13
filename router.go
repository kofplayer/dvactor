package dvactor

import (
	"strconv"
	"strings"

	"github.com/kofplayer/vactor"
)

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

func (r *Router) Router(envelope vactor.Envelope) vactor.VAError {
	var err vactor.VAError
	switch e := envelope.(type) {
	case *vactor.EnvelopeBatchSend:
		groups := make(map[vactor.SystemId][]vactor.ActorRef)
		for _, actorRef := range e.ToActorRefs {
			systemId := actorRef.GetSystemId()
			groups[systemId] = append(groups[systemId], actorRef)
		}
		for systemId, actorRefs := range groups {
			ebs := &vactor.EnvelopeBatchSend{
				FromActorRef: e.FromActorRef,
				ToActorRefs:  actorRefs,
				Messages:     e.Messages,
			}
			if systemId == r.systemId {
				r.system.LocalRouter(ebs)
			} else if sendErr := r.clusterNet.Send(systemId, ebs); sendErr != nil {
				if err == nil {
					err = sendErr
				} else {
					r.system.LogError("router err: %v", sendErr)
				}
			}
		}
	case *vactor.EnvelopeWatch:
		systemId := e.ToActorRef.GetSystemId()
		if systemId == r.systemId {
			r.system.LocalRouter(e)
		} else {
			if e.FromActorRef.GetActorType() == WatchProxyActorType {
				err = r.clusterNet.Send(systemId, e)
			} else {
				r.system.LocalRouter(&vactor.EnvelopeSend{
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
		groups := make(map[vactor.SystemId][]vactor.ActorRef)
		for _, actorRef := range e.ToActorRefs {
			systemId := actorRef.GetSystemId()
			groups[systemId] = append(groups[systemId], actorRef)
		}
		for systemId, actorRefs := range groups {
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
				r.system.LocalRouter(en)
			} else if sendErr := r.clusterNet.Send(systemId, en); sendErr != nil {
				if err == nil {
					err = sendErr
				} else {
					r.system.LogError("router err: %v", sendErr)
				}
			}
		}
	case *vactor.EnvelopeOuterWatch:
		systemId := e.ToActorRef.GetSystemId()
		if systemId == r.systemId {
			r.system.LocalRouter(e)
		} else {
			r.system.LocalRouter(&vactor.EnvelopeSend{
				Message: &OuterWatch{
					WatchType: e.WatchType,
					IsWatch:   e.IsWatch,
					Queue:     e.Queue,
				},
				ToActorRef: GetWatchProxyActorRef(r.system, r.systemId, e.ToActorRef),
			})
		}

	case *vactor.EnvelopeOuterRequest:
		systemId := e.ToActorRef.GetSystemId()
		if systemId == r.systemId {
			r.system.LocalRouter(e)
		} else {
			r.system.LocalRouter(&vactor.EnvelopeSend{
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
		systemId := envelope.GetToActorRef().GetSystemId()
		if systemId == r.systemId {
			r.system.LocalRouter(e)
		} else {
			err = r.clusterNet.Send(systemId, e)
		}
	}
	if err != nil {
		r.system.LogError("router err: %v\n", err)
	}
	return err
}
