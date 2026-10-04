package replay

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strconv"
	"time"
)

type CS2Launch struct {
	Phase   string `json:"phase"`
	Message string `json:"message"`
}

func launchPort(address string) (int, error) {
	host, p, err := net.SplitHostPort(address)
	if err != nil {
		return 0, errors.New("本机 NetCon 地址格式无效")
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return 0, errors.New("无头启动只支持本机 NetCon 地址")
	}
	port, err := strconv.Atoi(p)
	if err != nil || port < 1024 || port > 65535 {
		return 0, errors.New("NetCon 端口须为 1024–65535")
	}
	return port, nil
}
func (a *Service) startHeadlessCS2() error { return a.startHeadlessCS2WithMove(false) }
func (a *Service) startHeadlessCS2WithMove(move bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.role != "agent" {
		return errors.New("请在 Linux Agent 启动 CS2")
	}
	if a.active || a.cs2Launch.Phase == "starting" || a.localDemo.Phase == "armed" || a.localDemo.Phase == "running" {
		return errors.New("录制、Demo 测试或游戏启动正在进行，请先完成或停止")
	}
	port, err := launchPort(a.s.Config.NetCon)
	if err != nil {
		return err
	}
	lock := a.s.Config.SessionLock
	if lock == "" {
		lock = filepath.Join(a.dir, "cs2-close.lock")
	}
	release, err := resourceLock(lock)
	if err != nil {
		return err
	}
	address := a.s.Config.NetCon
	a.cs2Launch = CS2Launch{"starting", "正在无头显示器 :20 启动 CS2，等待 NetCon 响应…"}
	a.resetDemoTimelineLocked()
	a.localDemo = LocalDemo{Phase: "stopped"}
	a.saveLocked()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer release()
		ctx, cancel := context.WithTimeout(a.ctx, 90*time.Second)
		defer cancel()
		err := launchHeadlessCS2WithMove(ctx, port, address, move)
		if err == nil && a.recordingHUDCommand() == teamHUDCommand {
			err = hideGameUI(address)
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if err != nil {
			a.cs2Launch = CS2Launch{"failed", err.Error()}
			a.logLocked("warn", "无头启动 CS2 失败："+err.Error())
		} else {
			a.cs2Launch = CS2Launch{"ready", "CS2 已在无头显示器 :20 启动；NetCon " + address + " 命令回读成功"}
			a.logLocked("info", a.cs2Launch.Message)
		}
		a.saveLocked()
	}()
	return nil
}
