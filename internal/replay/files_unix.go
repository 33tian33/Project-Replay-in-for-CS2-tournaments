//go:build !windows

package replay

import "os"

func replaceFile(src, dst string) error { return os.Rename(src, dst) }
