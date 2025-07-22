package common

import (
	"fmt"
	"time"

	"github.com/kofplayer/dvactor"
	"github.com/kofplayer/vactor"
	"google.golang.org/protobuf/proto"
)

func TestSend(systemId vactor.SystemId) {
	TestActorType := dvactor.ActorTypeStart + 1

	system := dvactor.NewSystem(&dvactor.ClusterConfig{
		LocalSystemId: systemId,
		SystemConfigs: []*dvactor.SystemConfig{
			{
				SystemId:   1,
				Host:       "localhost",
				Port:       8001,
				ActorTypes: []vactor.ActorType{vactor.EventHubActorType, TestActorType},
			},
			{
				SystemId:   2,
				Host:       "localhost",
				Port:       8002,
				ActorTypes: []vactor.ActorType{vactor.EventHubActorType, TestActorType},
			},
		},
	})

	system.RegisterActorType(TestActorType, func() vactor.Actor {
		return func(ctx vactor.EnvelopeContext) {
			switch m := ctx.GetMessage().(type) {
			case *vactor.MsgOnStart:
				// inner send
				ctx.Send(ctx.GetActorRef(), &TestMessage{
					Msg: "hello from self",
				})

				// inner batch send
				actorRefs := make([]vactor.ActorRef, 100)
				for i := 0; i < len(actorRefs); i++ {
					actorRefs[i] = ctx.CreateActorRef(TestActorType, vactor.ActorId(fmt.Sprintf("%v", i)))
				}
				ctx.BatchSend(actorRefs, []interface{}{
					&TestMessage{
						Msg: "inner hello1",
					},
					&TestMessage{
						Msg: "inner hello2",
					},
				})
			case *TestMessage:
				ctx.LogDebug("Received message: %s", m.Msg)
			}
		}
	})

	system.RegisterMessageType(uint32(MessageType_MessageTypeTestMessage), func() proto.Message {
		return &TestMessage{}
	})

	system.Start()

	// outer send
	system.Send(system.CreateActorRef(TestActorType, "1"), &TestMessage{
		Msg: "hello from outer",
	})

	time.Sleep(time.Second)

	// outer batch send
	actorRefs := make([]vactor.ActorRef, 100)
	for i := 0; i < len(actorRefs); i++ {
		actorRefs[i] = system.CreateActorRef(TestActorType, vactor.ActorId(fmt.Sprintf("%v", i)))
	}
	system.BatchSend(actorRefs, []interface{}{
		&TestMessage{
			Msg: "outer hello1 from: " + fmt.Sprint(systemId),
		},
		&TestMessage{
			Msg: "outer hello2 from: " + fmt.Sprint(systemId),
		},
	})
	time.Sleep(time.Second * 10)

	system.Stop()
}
