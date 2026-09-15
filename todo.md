# 遗留事项

> 本文件的历史条目（重连后刷新 WatchProxy 的 watch）已于 2026-09-13 解决：重连成功
> （client 注册成功 / server 收到重新注册）触发 `onSystemReconnected`，本机 WatchProxy
> 对仍有订阅者的 WatchType 重新发起 watch。见 `system.go` 与 `watch_proxy.go`，
> 回归测试 `lifecycle_test.go` 的 `TestClusterWatchSubscriptionSurvivesPartition`。

新的待办请直接记入 [reports/](../reports/) 下的检查报告——那里有完整的背景、定级与
修复计划，比一份孤立清单更容易追溯。
