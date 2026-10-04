//go:build !linux

package replay

import "fmt"

func resourceLock(session string) (func(), error) {
	return nil, fmt.Errorf("真实采集仅在 Linux Agent 执行；Windows 请配置节点")
}
