# 遗留事项

~~system断开重连后watchProxy，根据情况需要去刷新watch~~
已解决：重连成功（client 注册成功 / server 收到重新注册）触发 `onSystemReconnected`，
本机 WatchProxy 对仍有订阅者的 WatchType 重新发起 watch。见 `system.go` 与 `watch_proxy.go`，
回归测试 `lifecycle_test.go` 的 `TestClusterWatchSubscriptionSurvivesPartition`。
