//go:build !linux

package replay

import "errors"

func terminateCS2() ([]int, error) { return nil, errors.New("一键关闭 CS2 目前仅支持 Linux") }
