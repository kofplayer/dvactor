# vactor

dvactor 是 [vactor](https://github.com/kofplayer/vactor) 的一个分布式扩展。能够把多个节点上的actor系统组成集群，协同工作。vactor所有功能在都能在分布式环境下使用。

## 序列化

- 使用了protobuf作为消息序列化。所有自定义消息都需要使用proto定义的消息。

## actor放置

- 不指定SystemId的情况下(CreateActorRef)，系统会通过actor id的hash值选择支持的节点放置actor
- 指定SystemId的情况下(CreateActorRefEx)，系统会把消息发送给System所在的节点。这样可能会出现相同type和id的actor，在多个节点中同时存在。

## 集群拓扑

- 集群中所有节点的拓扑配置在集群启动时确定，中途不能做修改。如果需要修改，就要先关闭整个集群。所以集群不支持运行时动态的添加和移除节点。

## 安装
d
```sh
go get github.com/kofplayer/dvactor
```

## 运行示例

终端1
```sh
go run ./examples/multi/system1
```

终端2
```sh
go run ./examples/multi/system2
```

## License

MIT License. 详情见 [`LICENSE`](LICENSE)。