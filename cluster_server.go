package dvactor

import (
	"fmt"
	"sync/atomic"

	socketNetConnect "github.com/kofplayer/dvactor/engine/net/connect/socket"
	netServer "github.com/kofplayer/dvactor/engine/net/server"
	netSession "github.com/kofplayer/dvactor/engine/net/session"
	"github.com/kofplayer/dvactor/protocol"
	"github.com/kofplayer/vactor"
	"google.golang.org/protobuf/proto"
)

type clusterServer struct {
	svr netServer.NetServer
	cn  *clusterNet
}

func (svr *clusterServer) OnConnect(s netSession.NetSession) {
}

func (svr *clusterServer) OnDisconnect(s netSession.NetSession) {
	bind := s.GetBindObject()
	if bind == nil {
		return
	}
	info, ok := bind.(*systemInfo)
	if !ok {
		return
	}
	info.lock.Lock()
	defer info.lock.Unlock()
	if info.session != s {
		return
	}
	s.SetBindObject(nil)
	info.session = nil
	atomic.AddInt32(&svr.cn.connectedSystemCount, -1)
	fmt.Printf("system %v disconnected\n", info.config.SystemId)
}

func (svr *clusterServer) OnMessage(s netSession.NetSession, msgId uint32, data []byte) error {
	switch protocol.PkgType(msgId) {
	case protocol.PkgType_PkgTypeRegisterSystemReq:
		req := &protocol.PkgRegisterSystemReq{}
		err := proto.Unmarshal(data, req)
		if err != nil {
			return err
		}
		info, ok := svr.cn.systemInfos[vactor.SystemId(req.SystemId)]
		if !ok {
			return fmt.Errorf("can not find systemId %v", req.SystemId)
		}
		if info.passive {
			return fmt.Errorf("systemId %v is passive", req.SystemId)
		}
		info.lock.Lock()
		defer info.lock.Unlock()
		if info.session != nil {
			return fmt.Errorf("systemId %v alreay register", req.SystemId)
		}
		info.session = s
		s.SetBindObject(info)
		atomic.AddInt32(&svr.cn.connectedSystemCount, 1)
		rsp := &protocol.PkgRegisterSystemRsp{
			ErrorCode: protocol.ErrorCode_ErrorCodeSuccess,
		}
		data, err := proto.Marshal(rsp)
		if err != nil {
			return err
		}
		fmt.Printf("system %v connected\n", req.SystemId)
		s.SendMessage(uint32(protocol.PkgType_PkgTypeRegisterSystemRsp), data)
		return nil
	default:
		return svr.cn.OnMessage(msgId, data)
	}
}

func (svr *clusterServer) Start() error {
	go func() {
		err := svr.svr.Start()
		if err != nil {
			panic(err)
		}
	}()
	return nil
}

func NewServer(cn *clusterNet) *clusterServer {
	svr := &clusterServer{
		cn: cn,
	}
	port := cn.systemInfos[cn.clusterConfig.LocalSystemId].config.Port
	svr.svr = netServer.NewNetServer()
	acceptor := socketNetConnect.NewAcceptor()
	acceptor.SetAddress("", port)
	svr.svr.SetAcceptor(acceptor)
	svr.svr.SetOnConnect(svr.OnConnect)
	svr.svr.SetOnDisconnect(func(s netSession.NetSession) {
		svr.OnDisconnect(s)
	})
	svr.svr.SetOnMessage(func(s netSession.NetSession, msgId uint32, data []byte) error {
		err := svr.OnMessage(s, msgId, data)
		if err != nil {
			fmt.Println(err)
			s.Close()
		}
		return err
	})
	return svr
}
