//go:build !linux

package bootstraprunner

import (
	"errors"
	"os/exec"
)

var errUnsupportedJobProcess = errors.New("bootstrap shell execution requires Linux per-job process groups")

func configureJobProcess(_ *exec.Cmd) (func(), error) {
	return nil, errUnsupportedJobProcess
}
