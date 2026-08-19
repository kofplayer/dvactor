module github.com/kofplayer/dvactor

go 1.24

require google.golang.org/protobuf v1.36.6

require github.com/kofplayer/vactor v0.0.0-20260819103222-6ec65fb5

// 本地联调：dvactor 依赖工作区内的 vactor 源码。发布时可移除此行并 go mod tidy。
replace github.com/kofplayer/vactor => ../vactor
