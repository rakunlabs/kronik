//go:build !unix

package executor

import "os/exec"

func processGroup(*exec.Cmd) func() {
	return func() {}
}
