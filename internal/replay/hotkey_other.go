//go:build !windows

package replay

func (a *Service) startHotkeys() {
	a.hotkeyStatus = "全局快捷键仅支持 Windows；可使用界面播出按钮"
}
