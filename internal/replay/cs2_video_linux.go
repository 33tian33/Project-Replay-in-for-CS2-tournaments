package replay

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// Keep the OBS capture resolution while minimizing rendering quality.
var lowVideoSettings = map[string]string{
	"setting.shaderquality": "0", "setting.r_texturefilteringquality": "0",
	"setting.msaa_samples": "0", "setting.r_csgo_cmaa_enable": "0",
	"setting.videocfg_shadow_quality": "0", "setting.videocfg_dynamic_shadows": "0",
	"setting.videocfg_texture_detail": "0", "setting.videocfg_particle_detail": "0",
	"setting.videocfg_ao_detail": "0", "setting.videocfg_hdr_detail": "3",
	"setting.videocfg_fsr_detail": "4", "setting.defaultres": "1920",
	"setting.defaultresheight": "1080", "setting.mat_vsync": "0",
}

func configureLowVideo(home string) error {
	running, err := headlessGameRunning()
	if err != nil {
		return err
	}
	if running {
		return fmt.Errorf("CS2 已运行，不能在游戏运行期间修改画质；请关闭后重试")
	}
	paths, err := filepath.Glob(filepath.Join(home, "snap/steam/common/.local/share/Steam/userdata/*/730/local/cfg/cs2_video.txt"))
	if err != nil {
		return err
	}
	if len(paths) != 1 {
		return fmt.Errorf("无法唯一确定 CS2 账户的视频配置（找到 %d 个）；请先在 Steam 中启动一次目标账户的 CS2 并退出", len(paths))
	}
	return applyLowVideo(paths[0])
}

func applyLowVideo(path string) error {
	original, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	updated := string(original)
	for key, value := range lowVideoSettings {
		re := regexp.MustCompile(`("` + regexp.QuoteMeta(key) + `"[\t ]+")[^"\r\n]*(")`)
		if len(re.FindAllStringIndex(updated, -1)) != 1 {
			return fmt.Errorf("视频配置缺少或重复字段 %s；原文件未修改", key)
		}
		updated = re.ReplaceAllStringFunc(updated, func(s string) string { m := re.FindStringSubmatch(s); return m[1] + value + m[2] })
	}
	if updated == string(original) {
		return nil
	}
	backup, err := os.CreateTemp(filepath.Dir(path), "cs2_video.txt.backup-*")
	if err != nil {
		return err
	}
	_, err = backup.Write(original)
	closeErr := backup.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".replay-video-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(info.Mode().Perm()); err == nil {
		_, err = f.WriteString(updated)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr = f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
