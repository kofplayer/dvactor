package dvactor

import (
	"fmt"

	"github.com/kofplayer/vactor"
)

const RequestProxyActorType vactor.ActorType = vactor.ActorTypeStart + 1

func GetRequestProxyActorRef(system vactor.System, systemId vactor.SystemId, toActorRef vactor.ActorRef) vactor.ActorRef {
	return system.CreateActorRefEx(systemId, RequestProxyActorType, vactor.ActorId(fmt.Sprintf("%v-%v", toActorRef.GetActorType(), toActorRef.GetActorId())))
}

func NewRequestProxy() *RequestProxy {
	return &RequestProxy{}
}

type RequestProxy struct {
}

type OuterRequest struct {
	ToActorRef vactor.ActorRef
	Message    interface{}
	RspChan    chan *vactor.Response
}

func (rp *RequestProxy) OnMessage(ctx vactor.EnvelopeContext) {
	switch m := ctx.GetMessage().(type) {
	case *OuterRequest:
		ctx.RequestAsync(m.ToActorRef, m.Message, 0, func(msg interface{}, err error) {
			m.RspChan <- &vactor.Response{
				Error:   err,
				Message: msg,
			}
		})
	}
}
