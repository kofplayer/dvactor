# vactor

dvactor 是 [vactor](https://github.com/kofplayer/vactor) 的一个分布式扩展。

## 说明

- vactor所有功能在都能在分布式环境下使用。
- 直接使用vactor的接口。vactor项目做很少修改就能支持分布式。
- 给使用CreateActorRef方法创建的actor ref发送消息时系统会自动选择节点创建actor。
- 给使用CreateActorRefEx方法创建的actor ref发送消息时系统会使用指定的节点。这时相同的type和id的actor可以同时存在多个节点上。
- 同一类型的actor不要混用CreateActorRef和CreateActorRefEx。
- 使用了protobuf作为消息序列化。所有自定义消息都需要使用proto定义的消息。

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