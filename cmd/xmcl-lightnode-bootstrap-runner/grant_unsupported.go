//go:build !linux

package main

import (
	"errors"
	"os"
)

func openProtectedGrant(path string) (*os.File, error) {
	return nil, errors.New("isolated test authorization requires Linux")
}
