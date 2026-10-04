package replay

import (
	"fmt"
	"os"
	"syscall"
)

func resourceLock(session string) (func(), error) {
	paths := []string{fmt.Sprintf("/tmp/cs2-recorder-%d.lock", os.Getuid())}
	if session != "" {
		paths = append([]string{session}, paths...)
	} else {
		return nil, fmt.Errorf("真实录制必须配置 CSStudio 的 session.lock 绝对路径")
	}
	files := []*os.File{}
	release := func() {
		for i := len(files) - 1; i >= 0; i-- {
			syscall.Flock(int(files[i].Fd()), syscall.LOCK_UN)
			files[i].Close()
		}
	}
	for _, p := range paths {
		f, e := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
		if e != nil {
			release()
			return nil, e
		}
		if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
			f.Close()
			release()
			return nil, fmt.Errorf("CSStudio/Replay 正在使用录制会话: %w", e)
		}
		files = append(files, f)
	}
	return release, nil
}

func dataLock(path string) (func(), error) {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, fmt.Errorf("数据目录已被另一 Replay 进程使用: %w", e)
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
