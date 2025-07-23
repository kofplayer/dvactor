[简体中文文档 (Chinese README)](ReadmeCh.md)

# vactor

dvactor is a distributed extension of [vactor](https://github.com/kofplayer/vactor). It enables actor systems on multiple nodes to form a cluster and work together. All vactor features are available in distributed environments.

## Serialization

- Uses protobuf for message serialization. All custom messages must be defined using proto.

## Actor Placement

- When SystemId is not specified (CreateActorRef), the system selects a node to place the actor based on the hash value of the actor id.
- When SystemId is specified (CreateActorRefEx), the system sends the message to the node where the System is located. In this case, actors with the same type and id may exist simultaneously on multiple nodes.

## Cluster Topology

- The topology configuration of all nodes in the cluster is determined at cluster startup and cannot be modified during runtime. If changes are needed, the entire cluster must be shut down first. Therefore, the cluster does not support dynamic addition or removal of nodes at runtime.


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