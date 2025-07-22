package common

import (
	"time"

	"github.com/kofplayer/dvactor"
	"github.com/kofplayer/vactor"
	"google.golang.org/protobuf/proto"
)

func TestRequest(systemId vactor.SystemId) {
	RequesterType := dvactor.ActorTypeStart + 1
	ResponserType := dvactor.ActorTypeStart + 2

	system := dvactor.NewSystem(&dvactor.ClusterConfig{
		LocalSystemId: systemId,
		SystemConfigs: []*dvactor.SystemConfig{
			{
				SystemId:   1,
				Host:       "localhost",
				Port:       8001,
				ActorTypes: []vactor.ActorType{vactor.EventHubActorType, RequesterType, ResponserType},
			},
			{
				SystemId:   2,
				Host:       "localhost",
				Port:       8002,
				ActorTypes: []vactor.ActorType{vactor.EventHubActorType, RequesterType, ResponserType},
			},
		},
	})

	system.RegisterActorType(RequesterType, func() vactor.Actor {
		return func(ctx vactor.EnvelopeContext) {
			switch ctx.GetMessage().(type) {
			case *vactor.MsgOnStart:
				// inner request
				rsp, err := ctx.Request(ctx.CreateActorRef(ResponserType, "1"), &TestReq{
					Msg: "req msg",
				}, 0)
				if err == nil {
					ctx.LogDebug("receive rsp: %v", rsp.(*TestRsp).Msg)
				} else {
					ctx.LogDebug("receive rsp error: %v", err)
				}
			}
		}
	})

	system.RegisterActorType(ResponserType, func() vactor.Actor {
		return func(ctx vactor.EnvelopeContext) {
			switch msg := ctx.GetMessage().(type) {
			case *TestReq:
				ctx.LogDebug("receive req: %v", msg.Msg)
				// response
				ctx.Response(&TestRsp{
					Msg: "rsp msg",
				}, nil)
			}
		}
	})

	system.RegisterMessageType(uint32(MessageType_MessageTypeTestReq), func() proto.Message {
		return &TestReq{}
	})

	system.RegisterMessageType(uint32(MessageType_MessageTypeTestRsp), func() proto.Message {
		return &TestRsp{}
	})

	system.Start()

	// active requester
	system.Send(system.CreateActorRef(RequesterType, "1"), &TestReq{
		Msg: "req msg",
	})

	// outer request
	rsp, err := system.Request(system.CreateActorRef(ResponserType, "1"), &TestReq{
		Msg: "req msg",
	}, 0)
	if err == nil {
		system.LogDebug("receive rsp: %v", rsp.(*TestRsp).Msg)
	} else {
		system.LogDebug("receive rsp error: %v", err)
	}

	time.Sleep(time.Second)
	system.Stop()
}
