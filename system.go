package dvactor

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/kofplayer/dvactor/protocol"
	"github.com/kofplayer/vactor"
	"google.golang.org/protobuf/proto"
)

const ActorTypeStart vactor.ActorType = vactor.ActorTypeStart + 10

type ClusterSystem interface {
	vactor.System
	RegisterMessageType(msgType uint32, creator func() proto.Message)
	// ClusterStartError 返回 Start() 阶段集群组网的错误：nil 表示全员互连成功，
	// 非 nil 表示等待互连超时或监听失败（Start 本身不返回错误，只会记日志）。
	ClusterStartError() error
}

func NewSystem(clusterConfig *ClusterConfig, cfgFuncs ...vactor.SystemConfigFunc) ClusterSystem {
	if err := validateClusterConfig(clusterConfig); err != nil {
		panic(fmt.Sprintf("invalid cluster config: %v", err))
	}
	cfgFuncs = append(cfgFuncs, func(sc *vactor.SystemConfig) {
		sc.SystemId = clusterConfig.LocalSystemId
	})
	_system := vactor.NewSystem(cfgFuncs...)
	s := &system{
		System:      _system,
		msgTypeIds:  make(map[reflect.Type]uint32),
		msgCreators: make(map[uint32]func() proto.Message),
	}
	s.clusterNet = NewClusterNet(s, clusterConfig)
	s.router = NewRouter(_system, clusterConfig, s.clusterNet)
	s.SetRouter(s.router.Router)
	s.SetCreateActorRefExFunc(s.router.CreateActorRefEx)
	s.System.RegisterActorType(WatchProxyActorType, func() vactor.Actor {
		return s.wrapWatchProxy(NewWatchProxy())
	})
	s.System.RegisterActorType(RequestProxyActorType, func() vactor.Actor {
		return NewRequestProxy().OnMessage
	})
	return s
}

// validateClusterConfig 校验集群配置：本地节点必须在列表中、SystemId 不得重复、
// 多节点时每个节点必须配置监听地址。无效配置在 NewSystem 时直接 panic，
// 避免到运行期才以 nil 解引用或无限等待的形式暴露。
func validateClusterConfig(clusterConfig *ClusterConfig) error {
	if clusterConfig == nil {
		return errors.New("cluster config is nil")
	}
	if len(clusterConfig.SystemConfigs) == 0 {
		return errors.New("system configs is empty")
	}
	seen := make(map[vactor.SystemId]bool)
	selfFound := false
	for _, cfg := range clusterConfig.SystemConfigs {
		if cfg == nil {
			return errors.New("system config is nil")
		}
		if seen[cfg.SystemId] {
			return fmt.Errorf("duplicate system id %v", cfg.SystemId)
		}
		seen[cfg.SystemId] = true
		if len(clusterConfig.SystemConfigs) > 1 && cfg.Port == 0 {
			return fmt.Errorf("system %v missing port in multi-node cluster", cfg.SystemId)
		}
		if cfg.SystemId == clusterConfig.LocalSystemId {
			selfFound = true
		}
	}
	if !selfFound {
		return fmt.Errorf("local system id %v not found in system configs", clusterConfig.LocalSystemId)
	}
	return nil
}

type SystemConfig struct {
	SystemId vactor.SystemId
	// Host 是其他节点连接本节点时使用的地址（client 侧目标地址）。
	Host string
	Port uint16
	// ListenHost 是本节点监听的网卡地址：空表示监听所有网卡（0.0.0.0），
	// 可设为 "127.0.0.1" 之类的回环地址以限制暴露面。
	ListenHost string
	ActorTypes []vactor.ActorType
}

type ClusterConfig struct {
	LocalSystemId vactor.SystemId
	SystemConfigs []*SystemConfig
	// ConnectTimeout: 启动时等待集群全员互连的超时时间；0 表示无限等待（保持旧行为）。
	ConnectTimeout time.Duration
	// AuthToken 注册握手共享密钥：非空时，注册请求必须携带相同 token 才会被接受，
	// 防止任意进程冒充节点接入。token 明文传输，仅作准入校验不提供机密性，
	// 跨公网部署请配合 TLS 或网络层隔离。所有节点必须配置一致的值。
	AuthToken string
}

type system struct {
	vactor.System
	router     *Router
	clusterNet *clusterNet
	// msgTypesLock 保护 msgTypeIds/msgCreators：注册可发生在 Start 后（虽不推荐），
	// 而编解码在网络 goroutine 中并发进行。
	msgTypesLock sync.RWMutex
	msgTypeIds   map[reflect.Type]uint32
	msgCreators  map[uint32]func() proto.Message
	// 类型解析缓存：序列化/反序列化是每条跨节点消息的必经路径，
	// 用无锁缓存避开反射 + RWMutex 的重复开销（注册时失效）。
	msgTypeCache    sync.Map // reflect.Type -> uint32
	msgCreatorCache sync.Map // uint32 -> func() proto.Message

	// clusterStartErr 记录 Start() 阶段集群组网的错误，经 ClusterStartError() 暴露。
	clusterStartErr error
	// stopOnce 保证集群网络层只关闭一次。
	stopOnce sync.Once

	// watchProxies 登记本机存活的 WatchProxy：key 为代理 actor 引用，
	// value 为其 watchee 引用。重连成功后按 watchee 所在系统刷新 watch。
	watchProxies sync.Map
}

func (s *system) Start() {
	s.System.Start()
	if err := s.clusterNet.start(); err != nil {
		s.clusterStartErr = err
		s.LogError("cluster net start failed: %v", err)
	}
}

// ClusterStartError 实现 ClusterSystem 接口。
func (s *system) ClusterStartError() error {
	return s.clusterStartErr
}

// Stop 停机：先关集群网络（停重连、关会话与监听），再停 actor 层。幂等。
func (s *system) Stop() {
	s.stopOnce.Do(func() {
		s.clusterNet.stop()
	})
	s.System.Stop()
}

// onSystemReconnected 在与远端系统的连接重新建立后调用（client 侧注册成功、
// server 侧收到重新注册都会触发）。通知本机以该远端系统为 watchee 的所有
// WatchProxy 重新发起 watch——分区期间发出的订阅、或远端重启丢失的订阅关系
// 由此自愈。
func (s *system) onSystemReconnected(remote vactor.SystemId) {
	s.watchProxies.Range(func(key, value any) bool {
		watchee := value.(vactor.ActorRefImpl)
		if watchee.SystemId != remote {
			return true
		}
		proxy := key.(vactor.ActorRefImpl)
		s.LocalRouter(&vactor.EnvelopeSend{
			FromActorRef: &proxy,
			ToActorRef:   &proxy,
			Message:      &watchProxyRefresh{},
		})
		return true
	})
}

// wrapWatchProxy 包装 WatchProxy 的消息入口：补充重连刷新消息的处理，
// 并在代理启动/停止时维护 watchProxies 登记表。
func (s *system) wrapWatchProxy(wp *WatchProxy) vactor.Actor {
	return func(ctx vactor.EnvelopeContext) {
		switch ctx.GetMessage().(type) {
		case *watchProxyRefresh:
			wp.refreshWatches(ctx)
		case *vactor.MsgOnStart:
			wp.OnMessage(ctx)
			if ref, ok := ctx.GetActorRef().(*vactor.ActorRefImpl); ok {
				if watchee, ok := wp.watcheeActorRef.(*vactor.ActorRefImpl); ok && watchee != nil {
					s.watchProxies.Store(*ref, *watchee)
				}
			}
		case *vactor.MsgOnStop:
			if ref, ok := ctx.GetActorRef().(*vactor.ActorRefImpl); ok {
				s.watchProxies.Delete(*ref)
			}
		default:
			wp.OnMessage(ctx)
		}
	}
}

func (s *system) RegisterActorType(actorType vactor.ActorType, actorCreator func() vactor.Actor) {
	if actorType < ActorTypeStart {
		panic(fmt.Sprintf("actor type %v is less than %v", actorType, ActorTypeStart))
	}
	s.System.RegisterActorType(actorType, actorCreator)
}

func (s *system) RegisterMessageType(msgType uint32, creator func() proto.Message) {
	msg := creator()
	msgReflectType := reflect.TypeOf(msg)

	s.msgTypesLock.Lock()
	defer s.msgTypesLock.Unlock()

	if old, ok := s.msgCreators[msgType]; ok && reflect.TypeOf(old()) != msgReflectType {
		s.LogWarn("msgType %v re-registered: %v -> %v", msgType, reflect.TypeOf(old()), msgReflectType)
	}
	if oldType, ok := s.msgTypeIds[msgReflectType]; ok && oldType != msgType {
		s.LogWarn("message %v re-registered with msgType %v (was %v)", msgReflectType, msgType, oldType)
	}
	s.msgTypeIds[msgReflectType] = msgType
	s.msgCreators[msgType] = creator
	// 覆盖注册后让缓存失效，避免读到旧的解析结果
	s.msgTypeCache.Delete(msgReflectType)
	s.msgCreatorCache.Delete(msgType)
}

func (s *system) MarshalMessage(msg interface{}) (*protocol.Message, vactor.VAError) {
	protoMsg, ok := msg.(proto.Message)
	if !ok {
		s.LogError("msg %v is not proto message", reflect.TypeOf(msg))
		return nil, vactor.NewVAError(ErrorCodeMessageCannotSerialize)
	}
	rt := reflect.TypeOf(msg)
	var msgType uint32
	if cached, hit := s.msgTypeCache.Load(rt); hit {
		msgType = cached.(uint32)
	} else {
		s.msgTypesLock.RLock()
		found, exists := s.msgTypeIds[rt]
		s.msgTypesLock.RUnlock()
		if !exists {
			s.LogError("can not find msg type %v", rt)
			return nil, vactor.NewVAError(ErrorCodeMessageNotRegister)
		}
		msgType = found
		s.msgTypeCache.Store(rt, found)
	}
	data, err := proto.Marshal(protoMsg)
	if err != nil {
		s.LogError("proto.Marshal %v", err)
		return nil, vactor.NewVAError(ErrorCodeMessageSerializeFail)
	}
	// Data 直接存放 proto 载荷；消息类型由 Type 字段承载
	//（历史上 Data 曾带 4 字节大端 msgType 前缀，与 Type 字段冗余，已移除）。
	return &protocol.Message{
		Type: msgType,
		Data: data,
	}, nil
}

func (s *system) UnmarshalMessage(protoMsg *protocol.Message) (interface{}, error) {
	if protoMsg == nil {
		return nil, errors.New("nil message")
	}
	var creator func() proto.Message
	if cached, hit := s.msgCreatorCache.Load(protoMsg.Type); hit {
		creator = cached.(func() proto.Message)
	} else {
		s.msgTypesLock.RLock()
		found, exists := s.msgCreators[protoMsg.Type]
		s.msgTypesLock.RUnlock()
		if !exists {
			return nil, fmt.Errorf("can not find msg type %v creator", protoMsg.Type)
		}
		creator = found
		s.msgCreatorCache.Store(protoMsg.Type, found)
	}
	msg := creator()
	if err := proto.Unmarshal(protoMsg.Data, msg); err != nil {
		return nil, err
	}
	return msg, nil
}
