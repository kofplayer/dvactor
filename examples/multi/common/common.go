package common

import (
	"github.com/kofplayer/vactor"
)

func Start(systemId vactor.SystemId) {
	// the follow test functions can only activate one at a time
	// TestSend(systemId)
	// TestRequest(systemId)
	// TestWatch(systemId)
	TestEvent(systemId)
}
