package netConnect

import "time"

// 心跳帧的 msgId：引擎层保留值（≤255，与业务 PkgType 1~11 无冲突），由引擎层在
// OnData 回调外自行处理，不进入业务层。
//
// 刻意是 const 而不是 var：它们在**每一帧**的回调里被读取（server/client 的
// SetOnData 闭包），做成包级可变变量就意味着"测试里改一下"会与运行中的连接构成
// 数据竞争——而这两个值本来就没有运行期可改的理由。
const (
	HeartbeatMsgIdPing = uint32(0xFE)
	HeartbeatMsgIdPong = uint32(0xFD)
)

// 引擎层心跳：双端按固定间隔发送 Ping，接收方对底层连接设置读超时——超过
// HeartbeatTimeout 未收到任何帧即判定连接死亡。这把半开连接的检测时间从 TCP 重传
// 超时（可达 15 分钟以上）缩短到 HeartbeatTimeout。
//
// 注意：以下变量应在建立连接之前配置（连接建立后并发修改有数据竞争）；
// 连接创建时会把这三个值快照进实例。
var (
	// HeartbeatInterval 双端发送 Ping 的间隔；<= 0 表示关闭心跳（同时不设读超时）。
	HeartbeatInterval = 20 * time.Second

	// HeartbeatTimeout 读超时时长；必须显著大于 HeartbeatInterval（覆盖数个
	// 心跳周期 + 一次网络抖动）。
	HeartbeatTimeout = 60 * time.Second
)

// 发送侧的上界（写超时、队列深度）与拨号超时见 config.go。
