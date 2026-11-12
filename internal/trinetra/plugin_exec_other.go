//go:build !linux

package trinetra

import "fmt"

func runPluginExec(id, binPath, tokenPath string) int {
	fmt.Fprintln(stderr, "trinetra plugin-exec: the sandboxed launcher is only available on linux")
	return 1
}
