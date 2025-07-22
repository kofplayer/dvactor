package common

import (
	"fmt"
	"time"

	"github.com/kofplayer/dvactor"
	"github.com/kofplayer/vactor"
	"google.golang.org/protobuf/proto"
)

func TestEvent(systemId vactor.SystemId) {
	system := dvactor.NewSystem(&dvactor.ClusterConfig{
		LocalSystemId: systemId,
		SystemConfigs: []*dvactor.SystemConfig{
			{
				SystemId:   1,
				Host:       "localhost",
				Port:       8001,
				ActorTypes: []vactor.ActorType{vactor.EventHubActorType},
			},
			{
				SystemId:   2,
				Host:       "localhost",
				Port:       8002,
				ActorTypes: []vactor.ActorType{vactor.EventHubActorType},
			},
		},
	})
	system.RegisterMessageType(uint32(MessageType_MessageTypeTestMessage), func() proto.Message {
		return &TestMessage{}
	})
	system.Start()

	eventGroup1 := vactor.EventGroup("eventGroup1")
	eventId1 := vactor.EventId(1)

	queue := vactor.NewQueue[interface{}]()
	system.ListenEvent(eventGroup1, eventId1, queue)
	go func() {
		for {
			m, ok := queue.Dequeue()
			if !ok {
				break
			}
			msg := m.(*vactor.MsgOnEventMsg)
			fmt.Printf("event from system %#v\n", msg.Message.(*TestMessage).Msg)
		}
	}()
	for range 10 {
		time.Sleep(time.Second)
		system.FireEvent(eventGroup1, eventId1, &TestMessage{
			Msg: fmt.Sprintf("%v", systemId),
		})
	}
	queue.Close()
	system.Stop()
}
