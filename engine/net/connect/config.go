package netConnect

import "time"

// 引擎层的发送与连接上界。与心跳参数同一条纪律：**应在建立连接之前配置**，
// 连接创建时会把用到的值快照进实例（见 socket.ConnSocket），运行期再改只影响
// 之后新建的连接。

var (
	// WriteTimeout 单帧写超时。对端停止读取（进程卡死、零窗口、网络黑洞）时，
	// 没有它 senderRun 会永久卡在 conn.Write 上：fd 不关、断线回调不触发、发送队列
	// 再无消费者，连接进入本端与对端都无法自愈的半死状态。
	// 取值要显著大于"健康链路写出一个最大帧"的时间，只用来识别真正的卡死。
	WriteTimeout = 30 * time.Second

	// SendQueueMaxFrames 单连接发送队列的帧数上限；<= 0 表示不限制（旧行为）。
	// 无界队列在慢对端下会把本端内存一直吃到 OOM，而 SendData 全程返回"成功"，
	// 上层据此误判消息已送达；心跳帧走同一队列，链路还会始终显示为健康。
	// 设界后队满才会在 SendData 上报错，上层得以降级（丢弃/计数/主动断链）。
	SendQueueMaxFrames = 8192

	// DialTimeout 主动连接侧的 TCP 拨号超时。此前用 net.Dial：黑洞地址会按内核
	// SYN 重试策略挂上数十秒到分钟级，期间 clusterClient 主循环停在 Connect() 上，
	// Stop() 也打不断它——停机被一个不可达节点拖住。
	DialTimeout = 10 * time.Second
)
