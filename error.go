package dvactor

import (
	"github.com/kofplayer/dvactor/protocol"
	"github.com/kofplayer/vactor"
)

const (
	ErrorCodeMessageCannotSerialize vactor.ErrorCode = vactor.ErrorCodeCustomStart + 1
	ErrorCodeMessageNotRegister     vactor.ErrorCode = vactor.ErrorCodeCustomStart + 2
	ErrorCodeMessageSerializeFail   vactor.ErrorCode = vactor.ErrorCodeCustomStart + 3
	ErrorCodeMessageLenError        vactor.ErrorCode = vactor.ErrorCodeCustomStart + 4
	ErrorCodeUnknownEnvelope        vactor.ErrorCode = vactor.ErrorCodeCustomStart + 5
	ErrorCodeMessageSendFail        vactor.ErrorCode = vactor.ErrorCodeCustomStart + 6
	ErrorCodeCustomStart            vactor.ErrorCode = vactor.ErrorCodeCustomStart + 100
)

// errorCodeToVAError 把线协议错误码还原为 VAError；成功码返回 nil（保持 err == nil 语义）。
func errorCodeToVAError(code protocol.ErrorCode) vactor.VAError {
	if code == protocol.ErrorCode_ErrorCodeSuccess {
		return nil
	}
	return vactor.NewVAError(vactor.ErrorCode(code))
}
