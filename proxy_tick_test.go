package dvactor

import (
	"testing"

	"github.com/kofplayer/vactor"
)

// tickSpyCtx 只实现被测代码用到的那几个 EnvelopeContext 方法，用来观察代理在
// OnStart 阶段有没有声明"不需要周期 tick"。
//
// 为什么需要这个测试：删掉 SetTickEnabled(false) 不会让任何功能出错（tick 处理器
// 本来就是空分支），只会让每个代理 actor 每秒被无谓唤醒。行为测试抓不到这种退化，
// 只能在意图层面锁住。
type tickSpyCtx struct {
	vactor.EnvelopeContext
	msg      interface{}
	disabled bool
}

func (c *tickSpyCtx) GetMessage() interface{} { return c.msg }

func (c *tickSpyCtx) SetTickEnabled(enabled bool) { c.disabled = !enabled }

func (c *tickSpyCtx) GetActorRef() vactor.ActorRef {
	// 代理 actorId 形如 "<actorType>-<actorId>"，这里给一个可解析的值
	return &vactor.ActorRefImpl{ActorType: WatchProxyActorType, ActorId: "20-target"}
}

func (c *tickSpyCtx) CreateActorRef(actorType vactor.ActorType, actorId vactor.ActorId) vactor.ActorRef {
	return &vactor.ActorRefImpl{ActorType: actorType, ActorId: actorId}
}

func TestWatchProxyDisablesPeriodicTick(t *testing.T) {
	c := &tickSpyCtx{msg: &vactor.MsgOnStart{}}
	NewWatchProxy().OnMessage(c)
	if !c.disabled {
		t.Fatal("WatchProxy must declare SetTickEnabled(false) on start: it has no MsgOnTick branch")
	}
}

func TestRequestProxyDisablesPeriodicTick(t *testing.T) {
	c := &tickSpyCtx{msg: &vactor.MsgOnStart{}}
	NewRequestProxy().OnMessage(c)
	if !c.disabled {
		t.Fatal("RequestProxy must declare SetTickEnabled(false) on start: it has no MsgOnTick branch")
	}
}
