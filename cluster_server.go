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
	svr      netServer.NetServer
	acceptor *socketNetConnect.AcceptorSocket
	cn       *clusterNet
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
	svr.cn.localSystem.LogInfo("system %v disconnected", info.config.SystemId)
}

func (svr *clusterServer) OnMessage(s netSession.NetSession, msgId uint32, data []byte) error {
	switch protocol.PkgType(msgId) {
	case protocol.PkgType_PkgTypeRegisterSystemReq:
		req := &protocol.PkgRegisterSystemReq{}
		err := proto.Unmarshal(data, req)
		if err != nil {
			return err
		}
		// 鉴权：配置了共享密钥时，token 不匹配的注册直接拒绝（会话随即关闭）
		if token := svr.cn.clusterConfig.AuthToken; token != "" && req.GetAuthToken() != token {
			svr.cn.localSystem.LogError("system %v register rejected: auth token mismatch", req.SystemId)
			// 先回带错误码的响应再断开：让 client 立即失败重试，
			// 而不是白等 registerResponseTimeout（10s）才察觉。
			if rspData, err := proto.Marshal(&protocol.PkgRegisterSystemRsp{
				ErrorCode: protocol.ErrorCode_ErrorCodeAuthFailed,
			}); err == nil {
				_ = s.SendMessage(uint32(protocol.PkgType_PkgTypeRegisterSystemRsp), rspData)
			}
			return fmt.Errorf("systemId %v auth rejected", req.SystemId)
		}
		info, ok := svr.cn.systemInfos[vactor.SystemId(req.SystemId)]
		if !ok {
			return fmt.Errorf("can not find systemId %v", req.SystemId)
		}
		if info.weDial {
			// 该节点本应由本节点主动去连：若它反过来连进来，说明两侧配置的方向不一致
			return fmt.Errorf("systemId %v should be dialed by us, not register to us", req.SystemId)
		}
		rsp := &protocol.PkgRegisterSystemRsp{
			ErrorCode: protocol.ErrorCode_ErrorCodeSuccess,
		}
		rspData, err := proto.Marshal(rsp)
		if err != nil {
			return err
		}
		info.lock.Lock()
		if info.session != nil {
			info.lock.Unlock()
			return fmt.Errorf("systemId %v already registered", req.SystemId)
		}
		info.session = s
		s.SetBindObject(info)
		atomic.AddInt32(&svr.cn.connectedSystemCount, 1)
		info.lock.Unlock()

		// 以下都在锁外：s.SendMessage 会走 TCP writer，onSystemReconnected 会向本机
		// 各 group 投递刷新消息。放在 info.lock 临界区里，一条慢链路或一次队列拥塞就能
		// 把该系统的重连与绑定清理一起拖住（doSend 拿的是同一把锁的读锁）。
		svr.cn.localSystem.LogInfo("system %v connected", req.SystemId)
		// 对端重新注册意味着连接经历过断开：触发本机代理 watch 刷新
		svr.cn.localSystem.onSystemReconnected(info.config.SystemId)
		_ = s.SendMessage(uint32(protocol.PkgType_PkgTypeRegisterSystemRsp), rspData)
		return nil
	default:
		return svr.cn.OnMessage(msgId, data)
	}
}

// Start 启动监听。通过 OnListen 回调同步感知端口绑定结果：
// 端口被占等监听失败会立即作为 error 返回，而不是被吞到后台日志里。
func (svr *clusterServer) Start() error {
	listenResult := make(chan error, 1)
	svr.acceptor.SetOnListen(func(err error) {
		listenResult <- err
	})
	go func() {
		if err := svr.svr.Start(); err != nil {
			// Accept 循环退出（含 Stop 关闭 listener）：记录即可，监听失败已在上面返回
			svr.cn.localSystem.LogError("cluster server stopped: %v", err)
		}
	}()
	return <-listenResult
}

// Stop 关闭监听与所有已建立的会话。会话关闭会触发各自的 OnDisconnect 清理绑定。
func (svr *clusterServer) Stop() {
	_ = svr.svr.Stop()
	svr.svr.GetSessionMgr().TravelSession(func(s netSession.NetSession) bool {
		_ = s.Close()
		return true
	})
}

func NewServer(cn *clusterNet) *clusterServer {
	svr := &clusterServer{
		cn: cn,
	}
	port := cn.systemInfos[cn.clusterConfig.LocalSystemId].config.Port
	svr.svr = netServer.NewNetServer()
	svr.acceptor = socketNetConnect.NewAcceptor()
	// ListenHost 为空时监听所有网卡（保持旧行为）
	svr.acceptor.SetAddress(cn.systemInfos[cn.clusterConfig.LocalSystemId].config.ListenHost, port)
	svr.svr.SetAcceptor(svr.acceptor)
	// 会话数上限：集群拓扑是静态的、全互联，本节点最多应有 (节点数-1) 条入站会话；
	// 留 4 倍余量给"旧链路尚未超时 + 新链路已接入"的重连重叠窗口。没有上限时，
	// 端口扫描或未注册的握手可以让 session 表与 fd 无界增长（回收要等 60s 读超时）。
	svr.svr.SetMaxSessions(4 * len(cn.clusterConfig.SystemConfigs))
	svr.svr.SetOnReject(func(remoteAddr string, limit int) {
		svr.cn.localSystem.LogError("reject cluster connection from %v: %d sessions already registered",
			remoteAddr, limit)
	})
	svr.svr.SetOnConnect(svr.OnConnect)
	svr.svr.SetOnDisconnect(func(s netSession.NetSession) {
		svr.OnDisconnect(s)
	})
	svr.svr.SetOnMessage(func(s netSession.NetSession, msgId uint32, data []byte) error {
		err := svr.cn.dispatchSafely(func() error { return svr.OnMessage(s, msgId, data) },
			"cluster server onMessage", msgId)
		if err != nil {
			svr.cn.localSystem.LogError("on message error: %v", err)
			// 出错即断开该会话；连接关闭后 server/client 双侧绑定均会被清理，
			// 对端按退避重连恢复
			_ = s.Close()
		}
		return err
	})
	return svr
}
