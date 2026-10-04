//go:build !linux

package replay

import (
	"context"
	"errors"
)

func launchHeadlessCS2(context.Context, int, string) error {
	return errors.New("无头启动仅支持 Linux")
}

func launchHeadlessCS2WithMove(ctx context.Context, port int, address string, move bool) error {
	return launchHeadlessCS2(ctx, port, address)
}
