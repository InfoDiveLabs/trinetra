package serverwatch

import (
	"os"
	"syscall"
)

var sighup os.Signal = syscall.SIGHUP
