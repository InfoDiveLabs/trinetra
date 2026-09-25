//go:build unix

package trinetra

import (
	"os"
	"syscall"
)

var sighup os.Signal = syscall.SIGHUP
