package dvactor

import (
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"

	"github.com/kofplayer/dvactor/protocol"
	"github.com/kofplayer/vactor"
	"google.golang.org/protobuf/proto"
)

const ActorTypeStart vactor.ActorType = vactor.ActorTypeStart + 10

type Error interface {
	error
	GetCode() int32
}

func NewError(code int32) Error {
	if code == 0 {
		return nil
	}
	return &gacError{
		code: code,
	}
}

type gacError struct {
	code int32
}

func (e *gacError) Error() string {
	return ""
}

func (e *gacError) GetCode() int32 {
	return e.code
}

type ClusterSystem interface {
	vactor.System
	RegisterMessageType(msgType uint32, creator func() proto.Message)
}

func NewSystem(clusterConfig *ClusterConfig, cfgFuncs ...vactor.SystemConfigFunc) ClusterSystem {
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
		return NewWatchProxy().OnMessage
	})
	s.System.RegisterActorType(RequestProxyActorType, func() vactor.Actor {
		return NewRequestProxy().OnMessage
	})
	return s
}

type SystemConfig struct {
	SystemId   vactor.SystemId
	Host       string
	Port       uint16
	ActorTypes []vactor.ActorType
}

type ClusterConfig struct {
	LocalSystemId vactor.SystemId
	SystemConfigs []*SystemConfig
}

type system struct {
	vactor.System
	router      *Router
	clusterNet  *clusterNet
	msgTypeIds  map[reflect.Type]uint32
	msgCreators map[uint32]func() proto.Message
}

func (s *system) Start() {
	s.System.Start()
	s.clusterNet.start()
}

func (s *system) RegisterActorType(actorType vactor.ActorType, actorCreator func() vactor.Actor) {
	if actorType < ActorTypeStart {
		panic(fmt.Sprintf("actor type %v is less than %v", actorType, ActorTypeStart))
	}
	s.System.RegisterActorType(actorType, actorCreator)
}

func (s *system) RegisterMessageType(msgType uint32, creator func() proto.Message) {
	msg := creator()
	s.msgTypeIds[reflect.TypeOf(msg)] = msgType
	s.msgCreators[msgType] = creator
}

func (s *system) MarshalMessage(msg interface{}) (*protocol.Message, error) {
	protoMsg, ok := msg.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("msg %v is not proto message", reflect.TypeOf(msg))
	}
	msgType, ok := s.msgTypeIds[reflect.TypeOf(msg)]
	if !ok {
		return nil, errors.New("can not find msg type")
	}
	_data, err := proto.Marshal(protoMsg)
	if err != nil {
		return nil, err
	}
	data := make([]byte, 4, 4+len(_data))
	binary.BigEndian.PutUint32(data[:4], msgType)
	data = append(data, _data...)
	return &protocol.Message{
		Type: msgType,
		Data: data,
	}, nil
}

func (s *system) UnmarshalMessage(protoMsg *protocol.Message) (interface{}, error) {
	if len(protoMsg.Data) < 4 {
		return nil, errors.New("len error")
	}
	creator, ok := s.msgCreators[protoMsg.Type]
	if !ok {
		return nil, fmt.Errorf("can not find msg type %v creator", protoMsg.Type)
	}
	msg := creator()
	err := proto.Unmarshal(protoMsg.Data[4:], msg)
	if err != nil {
		return nil, err
	}
	return msg, nil
}
