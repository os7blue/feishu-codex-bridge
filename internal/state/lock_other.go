//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package state

import (
	"errors"
	"os"
)

func acquireLock(string) (*os.File, error) {
	return nil, errors.New("this bridge currently requires a Unix host with flock support")
}
