package common

import (
	"time"

	"github.com/kofplayer/dvactor"
	"github.com/kofplayer/vactor"
	"google.golang.org/protobuf/proto"
)

func TestWatch(systemId vactor.SystemId) {
	wt := vactor.WatchType(111)
	WatcherType := dvactor.ActorTypeStart + 1
	WatcheeType := dvactor.ActorTypeStart + 2

	system := dvactor.NewSystem(&dvactor.ClusterConfig{
		LocalSystemId: systemId,
		SystemConfigs: []*dvactor.SystemConfig{
			{
				SystemId:   1,
				Host:       "localhost",
				Port:       8001,
				ActorTypes: []vactor.ActorType{vactor.EventHubActorType, WatcherType, WatcheeType},
			},
			{
				SystemId:   2,
				Host:       "localhost",
				Port:       8002,
				ActorTypes: []vactor.ActorType{vactor.EventHubActorType, WatcheeType, WatcheeType},
			},
		},
	})

	system.RegisterActorType(WatcherType, func() vactor.Actor {
		return func(ctx vactor.EnvelopeContext) {
			switch m := ctx.GetMessage().(type) {
			case *vactor.MsgOnStart:
				// inner watch
				ctx.Watch(system.CreateActorRef(WatcheeType, "1"), wt)
			case *vactor.MsgOnWatchMsg:
				ctx.LogDebug("inner watch: %v", m.Message.(*TestMessage).Msg)

				// inner unwatch
				ctx.Unwatch(system.CreateActorRef(WatcheeType, "1"), wt)
			}
		}
	})

	system.RegisterActorType(WatcheeType, func() vactor.Actor {
		return func(ctx vactor.EnvelopeContext) {
			switch ctx.GetMessage().(type) {
			case *vactor.MsgOnTick:
				// notify watcher
				ctx.Notify(wt, &TestMessage{
					Msg: "notify",
				})
			}
		}
	})

	system.RegisterMessageType(uint32(MessageType_MessageTypeTestMessage), func() proto.Message {
		return &TestMessage{}
	})

	system.Start()

	// outer watch
	queue := vactor.NewQueue[interface{}]()
	system.Watch(system.CreateActorRef(WatcheeType, "1"), wt, queue)
	go func() {
		for {
			m, ok := queue.Dequeue()
			if !ok {
				return
			}
			switch t := m.(*vactor.MsgOnWatchMsg).Message.(type) {
			case *TestMessage:
				system.LogDebug("outer watch: %v", t.Msg)
			}

			// outer unwatch
			system.Unwatch(system.CreateActorRef(WatcheeType, "1"), wt, queue)
			//queue.Close()
		}
	}()

	system.Send(system.CreateActorRef(WatcherType, "1"), &TestMessage{})

	time.Sleep(time.Second * 10)

	system.Stop()
}
