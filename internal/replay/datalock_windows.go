package replay

import (
	"fmt"
	"syscall"
)

func dataLock(path string) (func(), error) {
	p, e := syscall.UTF16PtrFromString(path)
	if e != nil {
		return nil, e
	}
	h, e := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if e != nil {
		return nil, fmt.Errorf("数据目录已被使用或无法写入: %w", e)
	}
	return func() { syscall.CloseHandle(h) }, nil
}
