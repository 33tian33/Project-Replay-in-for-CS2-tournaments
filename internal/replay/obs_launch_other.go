//go:build !linux

package replay

import (
	"context"
	"errors"
)

func launchHeadlessOBS(context.Context) (string, string, error) {
	return "", "", errors.New("无头 OBS 仅支持 Linux")
}
