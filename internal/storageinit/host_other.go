//go:build !linux

package storageinit

import (
	"context"
	"errors"
	"time"
)

const cleanupTimeout = 15 * time.Second

func openSystemVolume(context.Context, options, console) (volume, error) {
	return nil, errors.New("storage: preparation and unlock require a Linux host; no storage was changed")
}
