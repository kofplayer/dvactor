package dvactor

import (
	"fmt"
	"time"

	"github.com/kofplayer/vactor"
)

const RequestProxyActorType vactor.ActorType = vactor.ActorTypeStart + 1

// RequestProxyTimeout 代理转发请求的超时上限。
// 调用方（system.Request）自带超时控制，这里只是兜底，防止目标永不应答时代理 actor 无法回收。
const RequestProxyTimeout = time.Second * 30

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
		ctx.RequestAsync(m.ToActorRef, m.Message, RequestProxyTimeout, func(msg interface{}, err vactor.VAError) {
			// RspChan 容量为 1，调用方超时离开后写入也不会阻塞
			m.RspChan <- &vactor.Response{
				Error:   err,
				Message: msg,
			}
		})
	}
}
