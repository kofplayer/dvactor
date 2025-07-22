package main

import (
	"fmt"
	"time"

	"github.com/kofplayer/dvactor"
	"github.com/kofplayer/vactor"
)

func main() {
	actorType1 := dvactor.ActorTypeStart + 1
	actorType2 := dvactor.ActorTypeStart + 2
	system := dvactor.NewSystem(&dvactor.ClusterConfig{
		LocalSystemId: 1,
		SystemConfigs: []*dvactor.SystemConfig{
			{
				SystemId:   1,
				ActorTypes: []vactor.ActorType{actorType1, actorType2},
			},
		},
	})
	system.RegisterActorType(actorType1, func() vactor.Actor {
		return func(ec vactor.EnvelopeContext) {
			switch msg := ec.GetMessage().(type) {
			case int:
				fmt.Printf("recieve %v\n", msg)
			}
		}
	})
	system.Start()
	for i := 0; i < 1000; i++ {
		system.Send(system.CreateActorRef(actorType1, vactor.ActorId(fmt.Sprintf("%v", i))), i)
	}

	time.Sleep(time.Second)
}
