package replay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func steamLaunchPayload(port int) string {
	args := []string{"~/.steam/root/ubuntu12_32/steam", "-applaunch", "730", "-insecure", "-netconport", strconv.Itoa(port), "-condebug", "+cl_demo_predict", "0", "+spec_show_xray", "1", "-w", "1920", "-h", "1080"}
	for i, s := range args {
		args[i] = "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
	}
	return strings.Join(args, " ") + "\n"
}
func sendSteamLaunch(path string, port int) error {
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if err = syscall.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFIFO || st.Uid != uint32(os.Getuid()) {
		return errors.New("Steam 接收端必须是当前用户的命名管道")
	}
	payload := []byte(steamLaunchPayload(port))
	n, err := syscall.Write(fd, payload)
	if err != nil {
		return err
	}
	if n != len(payload) {
		return errors.New("Steam 启动请求未完整发送")
	}
	return nil
}
func headlessGameRunning() (bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, err
	}
	found := false
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !cs2Process(pid) {
			continue
		}
		env, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "environ"))
		if err != nil {
			return false, err
		}
		correct := false
		for _, v := range strings.Split(string(env), "\x00") {
			if v == "DISPLAY=:20" || v == "DISPLAY=:20.0" {
				correct = true
			}
		}
		if !correct {
			return false, errors.New("检测到其他显示器上的 CS2，请先关闭它再进行无头启动")
		}
		found = true
	}
	return found, nil
}
func launchHeadlessCS2(ctx context.Context, port int, address string) error {
	return launchHeadlessCS2WithMove(ctx, port, address, false)
}
func launchHeadlessCS2WithMove(ctx context.Context, port int, address string, move bool) error {
	clients, err := steamSnapshot()
	if err != nil {
		return err
	}
	if desktopSteam(clients) {
		if !move {
			return errors.New("Steam 正在桌面运行，会接管 CS2 启动；请点击“切换 Steam 到无头并启动”（会退出桌面 Steam）")
		}
		if err := moveSteamHeadless(ctx); err != nil {
			return err
		}
	}

	running, err := headlessGameRunning()
	if err != nil {
		return err
	}
	if !running {
		// Reuse the installed dedicated session, without restarting Steam/OBS/Xorg.
		output, err := exec.CommandContext(ctx, "systemctl", "--user", "show", "cs2-headless-steam.service", "--property=Environment", "--value").Output()
		if err != nil || !strings.Contains(string(output), "DISPLAY=:20") {
			return errors.New("未找到 DISPLAY=:20 的 cs2-headless-steam.service，请先配置本机无头 Steam 会话")
		}
		if err := exec.CommandContext(ctx, "systemctl", "--user", "start", "cs2-headless-steam.service").Run(); err != nil {
			return fmt.Errorf("启动无头 Steam 服务失败：%w", err)
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		if err := configureLowVideo(home); err != nil {
			return fmt.Errorf("配置最低画质失败：%w", err)
		}
		pipe := filepath.Join(home, "snap/steam/common/.steam/steam.pipe")
		for {
			if ctx.Err() != nil {
				return errors.New("等待无头 Steam 就绪超时，请检查 Steam 登录状态")
			}
			clients, scanErr := steamSnapshot()
			if scanErr != nil {
				return scanErr
			}
			if desktopSteam(clients) {
				return errors.New("桌面 Steam 再次接管启动，请退出桌面 Steam 后重试")
			}
			ready := false
			for _, client := range clients {
				if isSteamClient(client.args) && (client.display == ":20" || client.display == ":20.0") {
					ready = true
				}
			}
			if ready {
				err = sendSteamLaunch(pipe, port)
			} else {
				err = errors.New("等待无头 Steam 客户端")
			}
			if err == nil {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	for {
		if ctx.Err() != nil {
			return errors.New("CS2 启动或 NetCon 响应超时；请检查 Steam 登录、游戏错误，必要时一键关闭后重试")
		}
		running, err = headlessGameRunning()
		if err != nil {
			return err
		}
		if running {
			n, e := openConsole(address)
			if e == nil {
				_, e = n.queryWithTimeout("spec_show_xray 1; echo REPLAY_HEADLESS_READY", 2*time.Second)
				n.conn.Close()
				if e == nil {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
