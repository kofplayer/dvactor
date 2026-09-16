package dvactor

import (
	"fmt"
	"time"

	"github.com/kofplayer/vactor"
)

const RequestProxyActorType vactor.ActorType = vactor.ActorTypeStart + 1

// RequestProxyTimeout 代理转发请求的默认兜底超时：调用方未设时限时使用，
// 防止目标永不应答时代理 actor 无法回收。
const RequestProxyTimeout = time.Second * 30

// requestProxyTimeoutSlack 转发请求在调用方时限之上追加的余量，
// 保证代理总是晚于调用方超时，调用方能优先拿到真实结果/错误。
const requestProxyTimeoutSlack = 5 * time.Second

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
	// Timeout 是调用方的等待时限；代理的兜底超时取其加余量，防止对端
	// 永不应答时代理 actor 无法回收。
	Timeout time.Duration
}

func (rp *RequestProxy) OnMessage(ctx vactor.EnvelopeContext) {
	switch m := ctx.GetMessage().(type) {
	case *vactor.MsgOnStart:
		// 本代理不处理 MsgOnTick，关掉周期 tick。它依赖的异步超时扫描不受影响：
		// 转发期间 pendingAsyncCallback > 0，框架照常投递 tick（见 vactor 的 needTick）。
		ctx.SetTickEnabled(false)
	case *OuterRequest:
		// 代理兜底超时 = 调用方时限 + 余量；调用方未设时限时退回默认兜底值
		proxyTimeout := RequestProxyTimeout
		if m.Timeout > 0 {
			proxyTimeout = m.Timeout + requestProxyTimeoutSlack
		}
		ctx.RequestAsync(m.ToActorRef, m.Message, proxyTimeout, func(msg interface{}, err vactor.VAError) {
			// 非阻塞写：RspChan 容量为 1，正常路径（一次请求一次回调）不会满；
			// 但阻塞写会把"契约被破坏"变成"永久卡死代理 actor goroutine"，
			// 而非阻塞写只丢一条响应并留下一条可排查的日志。
			select {
			case m.RspChan <- &vactor.Response{
				Error:   err,
				Message: msg,
			}:
			default:
				ctx.LogWarn("request proxy response dropped: caller channel is full for %v", m.ToActorRef)
			}
		})
	}
}
