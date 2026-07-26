//go:build unix

package serverwatch

import (
	"os"
	"syscall"
)

var sighup os.Signal = syscall.SIGHUP
