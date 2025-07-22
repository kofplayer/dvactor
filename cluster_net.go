package dvactor

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	netClient "github.com/kofplayer/dvactor/engine/net/client"
	netSession "github.com/kofplayer/dvactor/engine/net/session"
	"github.com/kofplayer/dvactor/protocol"
	"github.com/kofplayer/vactor"
	"google.golang.org/protobuf/proto"
)

func NewClusterNet(localSystem *system, clusterConfig *ClusterConfig) *clusterNet {
	systemInfos := make(map[vactor.SystemId]*systemInfo)
	findSelf := false
	localSystemIndex := -1
	for i, config := range clusterConfig.SystemConfigs {
		if !findSelf && clusterConfig.LocalSystemId == config.SystemId {
			findSelf = true
			localSystemIndex = i
		}
		systemInfos[config.SystemId] = &systemInfo{
			config:  config,
			passive: !findSelf,
		}
	}
	return &clusterNet{
		localSystem:      localSystem,
		localSystemIndex: localSystemIndex,
		clusterConfig:    clusterConfig,
		systemInfos:      systemInfos,
		systemCount:      len(systemInfos),
	}
}

func ActorRefToProto(actorRef vactor.ActorRef) *protocol.ActorRef {
	if actorRef == nil {
		return nil
	}
	ref := actorRef.(*vactor.ActorRefImpl)
	return &protocol.ActorRef{
		SystemId:  uint32(ref.SystemId),
		GroupSlot: uint32(ref.GroupSlot),
		ActorType: uint32(ref.ActorType),
		ActorId:   string(ref.ActorId),
	}
}

func ActorRefFromProto(actorRef *protocol.ActorRef) vactor.ActorRef {
	if actorRef == nil {
		return nil
	}
	return &vactor.ActorRefImpl{
		SystemId:  vactor.SystemId(actorRef.SystemId),
		GroupSlot: vactor.GroupSlot(actorRef.GroupSlot),
		ActorType: vactor.ActorType(actorRef.ActorType),
		ActorId:   vactor.ActorId(actorRef.ActorId),
	}
}

type clusterNet struct {
	localSystem          *system
	localSystemIndex     int
	clusterConfig        *ClusterConfig
	systemInfos          map[vactor.SystemId]*systemInfo
	systemCount          int
	connectedSystemCount int32
	server               *clusterServer
	clients              map[vactor.SystemId]*clusterClient
}

type systemInfo struct {
	config  *SystemConfig
	passive bool
	lock    sync.RWMutex
	session netSession.NetSession
	cli     netClient.NetClient
}

func (cn *clusterNet) start() error {
	cn.localSystem.LogDebug("start cluster")
	atomic.AddInt32(&cn.connectedSystemCount, 1)
	if cn.localSystemIndex < cn.systemCount-1 {
		fmt.Println("start server")
		cn.server = NewServer(cn)
		if err := cn.server.Start(); err != nil {
			return err
		}
	}

	if cn.localSystemIndex != 0 {
		cn.clients = make(map[vactor.SystemId]*clusterClient)
		for index, config := range cn.clusterConfig.SystemConfigs {
			if index >= cn.localSystemIndex {
				break
			}
			client := NewClusterClient(cn, config.SystemId)
			cn.clients[config.SystemId] = client
			fmt.Printf("start client to %v\n", config.SystemId)
			client.Start()
		}
	}

	for {
		connectedSystemCount := atomic.LoadInt32(&cn.connectedSystemCount)
		if connectedSystemCount >= int32(cn.systemCount) {
			break
		}
		fmt.Printf("wait connected %v/%v\n", connectedSystemCount, cn.systemCount)
		time.Sleep(time.Second * 3)
	}

	fmt.Println("start cluster success")
	return nil
}

func (cn *clusterNet) doSend(systemId vactor.SystemId, msgId uint32, data []byte) error {
	info := cn.systemInfos[systemId]
	info.lock.RLock()
	defer info.lock.RUnlock()
	var err error
	if info.passive {
		if info.cli != nil {
			err = info.cli.SendMessage(msgId, data)
		} else {
			err = fmt.Errorf("system %v disconnect", systemId)
		}
	} else {
		if info.session != nil {
			err = info.session.SendMessage(msgId, data)
		} else {
			err = fmt.Errorf("system %v disconnect", systemId)
		}
	}
	return err
}

func (cn *clusterNet) Send(systemId vactor.SystemId, envelope vactor.Envelope) error {
	var msgId uint32
	var pkg proto.Message

	switch e := envelope.(type) {
	case *vactor.EnvelopeSend:
		msg, err := cn.localSystem.MarshalMessage(e.Message)
		if err != nil {
			return err
		}
		msgId = uint32(protocol.PkgType_PkgTypeEnvelopeSend)
		pkg = &protocol.PkgEnvelopeSend{
			FromActorRef: ActorRefToProto(e.FromActorRef),
			ToActorRef:   ActorRefToProto(e.ToActorRef),
			Message:      msg,
		}
	case *vactor.EnvelopeBatchSend:
		messages := make([]*protocol.Message, 0, len(e.Messages))
		for _, message := range e.Messages {
			msg, err := cn.localSystem.MarshalMessage(message)
			if err != nil {
				cn.localSystem.LogError("MarshalMessage error: %v\n", err)
				continue
			}
			messages = append(messages, msg)
		}
		if len(messages) <= 0 {
			return errors.New("no valid message can send")
		}
		stringActorRefs := make([]*protocol.ActorRef, len(e.ToActorRefs))
		for i, key := range e.ToActorRefs {
			stringActorRefs[i] = ActorRefToProto(key)
		}
		msgId = uint32(protocol.PkgType_PkgTypeEnvelopeBatchSend)
		pkg = &protocol.PkgEnvelopeBatchSend{
			FromActorRef: ActorRefToProto(e.FromActorRef),
			ToActorRefs:  stringActorRefs,
			Messages:     messages,
		}
	case *vactor.EnvelopeRequestAsync:
		msg, err := cn.localSystem.MarshalMessage(e.Message)
		if err != nil {
			return err
		}
		msgId = uint32(protocol.PkgType_PkgTypeEnvelopeRequestAsync)
		pkg = &protocol.PkgEnvelopeRequestAsync{
			FromActorRef:    ActorRefToProto(e.FromActorRef),
			ToActorRef:      ActorRefToProto(e.ToActorRef),
			Message:         msg,
			CallbackId:      uint32(e.CallbackId),
			CallbackAddress: e.CallbackAddress,
		}
	case *vactor.EnvelopeResponseAsync:
		msg, err := cn.localSystem.MarshalMessage(e.Message)
		if err != nil {
			return err
		}
		msgId = uint32(protocol.PkgType_PkgTypeEnvelopeResponseAsync)
		rsp := &protocol.Response{
			Message: msg,
		}
		if e.Error == nil {
			rsp.ErrorCode = protocol.ErrorCode_ErrorCodeSuccess
		} else {
			if realError, ok := e.Error.(Error); ok {
				rsp.ErrorCode = protocol.ErrorCode(realError.GetCode())
			} else {
				rsp.ErrorCode = protocol.ErrorCode_ErrorCodeNormal
			}
		}
		pkg = &protocol.PkgEnvelopeResponseAsync{
			FromActorRef:    ActorRefToProto(e.FromActorRef),
			ToActorRef:      ActorRefToProto(e.ToActorRef),
			Response:        rsp,
			CallbackId:      uint32(e.CallbackId),
			CallbackAddress: e.CallbackAddress,
		}
	case *vactor.EnvelopeRequest:
		msg, err := cn.localSystem.MarshalMessage(e.Message)
		if err != nil {
			return err
		}
		msgId = uint32(protocol.PkgType_PkgTypeEnvelopeRequest)
		pkg = &protocol.PkgEnvelopeRequest{
			FromActorRef: ActorRefToProto(e.FromActorRef),
			ToActorRef:   ActorRefToProto(e.ToActorRef),
			Message:      msg,
		}
	case *vactor.EnvelopeResponse:
		msg, err := cn.localSystem.MarshalMessage(e.Message)
		if err != nil {
			return err
		}
		msgId = uint32(protocol.PkgType_PkgTypeEnvelopeResponse)
		rsp := &protocol.Response{
			Message: msg,
		}
		if e.Error == nil {
			rsp.ErrorCode = protocol.ErrorCode_ErrorCodeSuccess
		} else {
			if realError, ok := e.Error.(Error); ok {
				rsp.ErrorCode = protocol.ErrorCode(realError.GetCode())
			} else {
				rsp.ErrorCode = protocol.ErrorCode_ErrorCodeNormal
			}
		}
		pkg = &protocol.PkgEnvelopeResponse{
			FromActorRef: ActorRefToProto(e.FromActorRef),
			ToActorRef:   ActorRefToProto(e.ToActorRef),
			Response:     rsp,
		}
	case *vactor.EnvelopeWatch:
		msgId = uint32(protocol.PkgType_PkgTypeEnvelopeWatch)
		pkg = &protocol.PkgEnvelopeWatch{
			FromActorRef: ActorRefToProto(e.FromActorRef),
			ToActorRef:   ActorRefToProto(e.ToActorRef),
			WatchType:    uint32(e.WatchType),
			IsWatch:      e.IsWatch,
		}
	case *vactor.EnvelopeNotify:
		msg, err := cn.localSystem.MarshalMessage(e.Message.Message)
		if err != nil {
			return err
		}
		msgId = uint32(protocol.PkgType_PkgTypeEnvelopeNotify)
		toActorRefs := make([]*protocol.ActorRef, len(e.ToActorRefs))
		for i, key := range e.ToActorRefs {
			toActorRefs[i] = ActorRefToProto(key)
		}
		pkg = &protocol.PkgEnvelopeNotify{
			FromActorRef: ActorRefToProto(e.FromActorRef),
			ToActorRefs:  toActorRefs,
			NotifyType:   uint32(e.NotifyType),
			ActorRef:     ActorRefToProto(e.Message.ActorRef),
			WatchType:    uint32(e.Message.WatchType),
			Message:      msg,
		}
	case *vactor.EnvelopeFireNotify:
		msg, err := cn.localSystem.MarshalMessage(e.Message)
		if err != nil {
			return err
		}
		msgId = uint32(protocol.PkgType_PkgTypeEnvelopeFireNotify)
		pkg = &protocol.PkgEnvelopeFireNotify{
			FromActorRef: ActorRefToProto(e.FromActorRef),
			ToActorRef:   ActorRefToProto(e.ToActorRef),
			NotifyType:   uint32(e.NotifyType),
			WatchType:    uint32(e.WatchType),
			Message:      msg,
		}
	default:
		return errors.New("unknown envelope type")
	}
	data, err := proto.Marshal(pkg)
	if err != nil {
		return err
	}
	return cn.doSend(systemId, msgId, data)
}

func (cn *clusterNet) OnMessage(msgId uint32, data []byte) error {
	switch protocol.PkgType(msgId) {
	case protocol.PkgType_PkgTypeEnvelopeSend:
		pkg := &protocol.PkgEnvelopeSend{}
		err := proto.Unmarshal(data, pkg)
		if err != nil {
			return err
		}
		msg, err := cn.localSystem.UnmarshalMessage(pkg.Message)
		if err != nil {
			return err
		}
		cn.localSystem.LocalRouter(&vactor.EnvelopeSend{
			FromActorRef: ActorRefFromProto(pkg.FromActorRef),
			ToActorRef:   ActorRefFromProto(pkg.ToActorRef),
			Message:      msg,
		})
	case protocol.PkgType_PkgTypeEnvelopeBatchSend:
		pkg := &protocol.PkgEnvelopeBatchSend{}
		err := proto.Unmarshal(data, pkg)
		if err != nil {
			return err
		}
		msgs := make([]interface{}, 0, len(pkg.Messages))
		for _, protoMsg := range pkg.Messages {
			msg, err := cn.localSystem.UnmarshalMessage(protoMsg)
			if err != nil {
				cn.localSystem.LogError("UnmarshalMessage error: %v", err)
				continue
			}
			msgs = append(msgs, msg)
		}
		toActorRefs := make([]vactor.ActorRef, len(pkg.ToActorRefs))
		for i, key := range pkg.ToActorRefs {
			toActorRefs[i] = ActorRefFromProto(key)
		}
		e := &vactor.EnvelopeBatchSend{
			FromActorRef: ActorRefFromProto(pkg.FromActorRef),
			ToActorRefs:  toActorRefs,
			Messages:     msgs,
		}
		cn.localSystem.LocalRouter(e)
	case protocol.PkgType_PkgTypeEnvelopeRequestAsync:
		pkg := &protocol.PkgEnvelopeRequestAsync{}
		err := proto.Unmarshal(data, pkg)
		if err != nil {
			return err
		}
		msg, err := cn.localSystem.UnmarshalMessage(pkg.Message)
		if err != nil {
			return err
		}
		e := &vactor.EnvelopeRequestAsync{
			FromActorRef:    ActorRefFromProto(pkg.FromActorRef),
			ToActorRef:      ActorRefFromProto(pkg.ToActorRef),
			Message:         msg,
			CallbackId:      vactor.CallbackId(pkg.CallbackId),
			CallbackAddress: pkg.CallbackAddress,
		}
		cn.localSystem.LocalRouter(e)
	case protocol.PkgType_PkgTypeEnvelopeResponseAsync:
		pkg := &protocol.PkgEnvelopeResponseAsync{}
		err := proto.Unmarshal(data, pkg)
		if err != nil {
			return err
		}
		var msg interface{}
		if pkg.Response.Message != nil {
			msg, err = cn.localSystem.UnmarshalMessage(pkg.Response.Message)
			if err != nil {
				return err
			}
		}
		e := &vactor.EnvelopeResponseAsync{
			FromActorRef: ActorRefFromProto(pkg.FromActorRef),
			ToActorRef:   ActorRefFromProto(pkg.ToActorRef),
			Response: &vactor.Response{
				Error:   NewError(int32(pkg.Response.ErrorCode)),
				Message: msg,
			},
			CallbackId:      vactor.CallbackId(pkg.CallbackId),
			CallbackAddress: pkg.CallbackAddress,
		}
		cn.localSystem.LocalRouter(e)
	case protocol.PkgType_PkgTypeEnvelopeRequest:
		pkg := &protocol.PkgEnvelopeRequest{}
		err := proto.Unmarshal(data, pkg)
		if err != nil {
			return err
		}
		msg, err := cn.localSystem.UnmarshalMessage(pkg.Message)
		if err != nil {
			return err
		}
		e := &vactor.EnvelopeRequest{
			FromActorRef: ActorRefFromProto(pkg.FromActorRef),
			ToActorRef:   ActorRefFromProto(pkg.ToActorRef),
			Message:      msg,
		}
		cn.localSystem.LocalRouter(e)
	case protocol.PkgType_PkgTypeEnvelopeResponse:
		pkg := &protocol.PkgEnvelopeResponse{}
		err := proto.Unmarshal(data, pkg)
		if err != nil {
			return err
		}
		var msg interface{}
		if pkg.Response.Message != nil {
			msg, err = cn.localSystem.UnmarshalMessage(pkg.Response.Message)
			if err != nil {
				return err
			}
		}
		e := &vactor.EnvelopeResponse{
			FromActorRef: ActorRefFromProto(pkg.FromActorRef),
			ToActorRef:   ActorRefFromProto(pkg.ToActorRef),
			Response: &vactor.Response{
				Error:   NewError(int32(pkg.Response.ErrorCode)),
				Message: msg,
			},
		}
		cn.localSystem.LocalRouter(e)
	case protocol.PkgType_PkgTypeEnvelopeWatch:
		pkg := &protocol.PkgEnvelopeWatch{}
		err := proto.Unmarshal(data, pkg)
		if err != nil {
			return err
		}
		e := &vactor.EnvelopeWatch{
			FromActorRef: ActorRefFromProto(pkg.FromActorRef),
			ToActorRef:   ActorRefFromProto(pkg.ToActorRef),
			WatchType:    vactor.WatchType(pkg.WatchType),
			IsWatch:      pkg.IsWatch,
		}
		cn.localSystem.LocalRouter(e)
	case protocol.PkgType_PkgTypeEnvelopeNotify:
		pkg := &protocol.PkgEnvelopeNotify{}
		err := proto.Unmarshal(data, pkg)
		if err != nil {
			return err
		}
		msg, err := cn.localSystem.UnmarshalMessage(pkg.Message)
		if err != nil {
			return err
		}
		toActorRefs := make([]vactor.ActorRef, len(pkg.ToActorRefs))
		for i, key := range pkg.ToActorRefs {
			toActorRefs[i] = ActorRefFromProto(key)
		}
		e := &vactor.EnvelopeNotify{
			FromActorRef: ActorRefFromProto(pkg.FromActorRef),
			ToActorRefs:  toActorRefs,
			NotifyType:   vactor.NotifyType(pkg.NotifyType),
			Message: &vactor.MsgOnWatchMsg{
				ActorRef:  ActorRefFromProto(pkg.ActorRef),
				WatchType: vactor.WatchType(pkg.WatchType),
				Message:   msg,
			},
		}
		cn.localSystem.LocalRouter(e)
	case protocol.PkgType_PkgTypeEnvelopeFireNotify:
		pkg := &protocol.PkgEnvelopeFireNotify{}
		err := proto.Unmarshal(data, pkg)
		if err != nil {
			return err
		}
		msg, err := cn.localSystem.UnmarshalMessage(pkg.Message)
		if err != nil {
			return err
		}
		e := &vactor.EnvelopeFireNotify{
			FromActorRef: ActorRefFromProto(pkg.FromActorRef),
			ToActorRef:   ActorRefFromProto(pkg.ToActorRef),
			NotifyType:   vactor.NotifyType(pkg.NotifyType),
			WatchType:    vactor.WatchType(pkg.WatchType),
			Message:      msg,
		}
		cn.localSystem.LocalRouter(e)
	}
	return nil
}
