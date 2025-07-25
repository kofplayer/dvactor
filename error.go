package dvactor

import "github.com/kofplayer/vactor"

const (
	ErrorCodeMessageCannotSerialize vactor.ErrorCode = vactor.ErrorCodeCustomStart + 1
	ErrorCodeMessageNotRegister     vactor.ErrorCode = vactor.ErrorCodeCustomStart + 2
	ErrorCodeMessageSerializeFail   vactor.ErrorCode = vactor.ErrorCodeCustomStart + 3
	ErrorCodeMessageLenError        vactor.ErrorCode = vactor.ErrorCodeCustomStart + 4
	ErrorCodeUnknownEnvelope        vactor.ErrorCode = vactor.ErrorCodeCustomStart + 5
	ErrorCodeMessageSendFail        vactor.ErrorCode = vactor.ErrorCodeCustomStart + 6
	ErrorCodeCustomStart            vactor.ErrorCode = vactor.ErrorCodeCustomStart + 100
)
