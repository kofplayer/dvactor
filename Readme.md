# vactor

[简体中文文档 (Chinese README)](ReadmeCh.md)

dvactor is a distributed extension of [vactor](https://github.com/kofplayer/vactor).

## Description

- All vactor features are available in distributed environments.
- Directly use vactor interfaces. The vactor project requires very few changes to support distribution.
- When sending messages to actor refs created with the CreateActorRef method, the system will automatically select a node to create the actor.
- When sending messages to actor refs created with the CreateActorRefEx method, the system will use the specified node. In this case, actors with the same type and id can exist on multiple nodes simultaneously.
- Do not mix CreateActorRef and CreateActorRefEx for actors of the same type.
- Uses protobuf for message serialization. All custom messages must be defined using proto.

## Installation

```sh
go get github.com/kofplayer/dvactor
```

## Example Usage

Terminal 1:
```sh
go run ./examples/multi/system1
```

Terminal 2:
```sh
go run ./examples/multi/system2
```

## License

MIT License. See [`LICENSE`](LICENSE) for details.