package replay

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed demo.mp4
var demoVideo []byte

func (a *Service) runJob(j Job, c Config) {
	artifacts, e := a.capture(j, c)
	if e != nil {
		a.jobUpdate(j.ID, "FAILED", labelErr(e), nil)
		return
	}
	a.jobUpdate(j.ID, "READY", "片段已校验，可供导播预览", artifacts)
}
func (a *Service) capture(j Job, c Config) ([]Artifact, error) {
	if e := a.waitUntil(j.Start - int64(c.Setup*1000)); e != nil {
		return nil, e
	}
	j = a.captureSnapshot(j, false)
	a.mu.Lock()
	valid := a.s.Config.Epoch == j.Epoch && a.s.Config.Match == j.Match && a.s.Config.Map == j.Map
	clockErr := a.checkRoundJobLocked(j)
	a.mu.Unlock()
	if clockErr != nil {
		return nil, clockErr
	}
	if !valid {
		return nil, errors.New("录制任务会话已失效")
	}
	for !a.captureMu.TryLock() {
		if nowMS() >= j.Start {
			return nil, errors.New("录制设备仍被占用，无法及时切换镜头")
		}
		if err := a.waitUntil(min(j.Start, nowMS()+10)); err != nil {
			return nil, err
		}
	}
	locked := true
	defer func() {
		if locked {
			a.captureMu.Unlock()
		}
	}()
	var cameraEvidence []trackingSample
	raw := ""
	origin := j.Start
	if j.Demo {
		a.jobUpdate(j.ID, "CAPTURING", "演示采集 · 不连接游戏和 OBS", nil)
		for {
			j = a.captureSnapshot(j, true)
			if j.Status == "STOPPING" {
				break
			}
			if e := a.waitUntil(min(j.End, nowMS()+50)); e != nil {
				return nil, e
			}
		}
	} else {
		if c.Strict && !j.Events[0].RoundTiming {
			return nil, errors.New("严格定位适配器尚未接入，无法发布精确片段")
		}
		release, e := resourceLock(c.SessionLock)
		if e != nil {
			return nil, e
		}
		defer func() {
			if release != nil {
				release()
			}
		}()

		o, e := openOBS(c)
		if e != nil {
			return nil, e
		}
		defer o.close()
		status, e := o.call("GetRecordStatus", nil)
		if e != nil {
			return nil, e
		}
		if status["outputActive"] == true {
			return nil, errors.New("OBS 已在录制，拒绝接管现有录制")
		}
		// Reapply the selected HUD mode before recording and after camera switches.
		if e = a.prepareRecordingHUD(o, c); e != nil {
			return nil, fmt.Errorf("录制前应用 HUD 设置失败: %w", e)
		}
		initial, shotAt := cameraJob(j, c, j.Start)
		camera, e := a.prepareCamera(initial, c)
		if e != nil {
			return nil, e
		}
		defer camera.Close()
		if j.Utility != nil {
			if err := netCommand(c.NetCon, a.recordingHUDCommand()); err != nil {
				return nil, err
			}
		}

		if _, e = o.call("StartRecord", nil); e != nil {
			return nil, e
		}
		stopped := false
		defer func() {
			if stopped {
				return
			}
			// An aborted camera check must not leave OBS recording and poison
			// every later job. Keep camera ownership until stop is confirmed.
			stopSent := false
			confirmed := false
			for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
				v, err := o.call("GetRecordStatus", nil)
				if err != nil {
					break
				}
				if v["outputActive"] == true && !stopSent {
					st, err := o.call("StopRecord", nil)
					if err != nil {
						break
					}
					a.rememberRaw(stringField(st, "outputPath"))
					stopSent = true
				} else if v["outputActive"] != true && stopSent {
					confirmed = true
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if !confirmed {
				a.mu.Lock()
				a.logLocked("warn", "录制异常后未确认 OBS 停止；请检查 OBS 连接和录制状态")
				a.mu.Unlock()
			}
		}()
		// StartRecord acknowledges the request before the encoder becomes active.
		for {
			status, e = o.call("GetRecordStatus", nil)
			if e != nil {
				return nil, e
			}
			if status["outputActive"] == true {
				break
			}
			if nowMS() >= j.Start {
				return nil, errors.New("OBS 未在保护窗口开始前进入录制状态")
			}
			if e = camera.Check(); e != nil {
				return nil, e
			}
			if e = a.waitUntil(min(j.Start, nowMS()+50)); e != nil {
				return nil, e
			}
		}
		origin = nowMS() - int64(number(status, "outputDuration"))
		if origin > j.Start {
			return nil, errors.New("MISSED_WINDOW：录制启动晚于保护窗口")
		}
		a.jobUpdate(j.ID, "CAPTURING", "切换镜头后立即录制；持续核验目标", nil)
		for {
			j = a.captureSnapshot(j, true)
			if j.Status == "STOPPING" {
				break
			}
			if player, ok := camera.(*playerCamera); ok {
				target, nextShotAt := cameraJob(j, c, nowMS())
				if nextShotAt != shotAt {
					if nowMS() >= target.Start {
						return nil, errors.New("MISSED_WINDOW：未能在目标击杀前直接切换视角")
					}
					next, err := a.prepareCamera(target, c)
					if err != nil {
						return nil, err
					}
					camera.Close()
					camera, shotAt = next, nextShotAt
				} else {
					player.job = target
				}
			}
			if e = camera.Check(); e != nil {
				return nil, e
			}

			if e = a.waitUntil(min(j.End, nowMS()+50)); e != nil {
				return nil, e
			}
		}
		cameraEvidence = camera.Evidence()
		status, e = o.call("StopRecord", nil)
		if e != nil {
			return nil, e
		}
		a.rememberRaw(stringField(status, "outputPath"))
		// StopRecord also acknowledges asynchronously. The encoder/muxer must
		// finish before ffmpeg opens the raw file or it can see a truncated tail.
		for deadline := nowMS() + 10000; ; {
			v, err := o.call("GetRecordStatus", nil)
			if err != nil {
				return nil, err
			}
			if v["outputActive"] != true {
				break
			}
			if nowMS() >= deadline {
				return nil, errors.New("OBS 停止录制超时")
			}
			if err = a.waitUntil(nowMS() + 50); err != nil {
				return nil, err
			}
		}
		stopped = true
		camera.Close()
		release()
		release = nil
		raw = stringField(status, "outputPath")
		if raw == "" {
			return nil, errors.New("OBS 未返回录制文件路径")
		}
	}
	a.captureMu.Unlock()
	locked = false
	a.jobUpdate(j.ID, "FINALIZING", "按实际镜头窗口生成击杀片段并校验 SHA-256", nil)
	artifacts := []Artifact{}
	for _, event := range j.Events {
		artifactID := j.ID + "-" + event.ID
		path := filepath.Join(a.dir, "media", artifactID+".mp4")
		temp := path + ".tmp.mp4"
		duration := replayDuration
		if j.Demo {
			// Built-in synthetic sample is validated by the release test; portable demo needs no codecs installed.
			if e := os.WriteFile(temp, demoVideo, 0600); e != nil {
				return nil, e
			}
		} else {
			clipStart, clipEnd := eventClipBounds(j, event, c)
			if clipStart > event.Time-cameraBeforeMS || clipEnd < event.Time+cameraAfterMS {
				return nil, errors.New("素材未覆盖击杀的有效镜头窗口")
			}
			wantedDuration := float64(clipEnd-clipStart) / 1000
			offset := float64(clipStart-origin) / 1000
			if offset < 0 {
				return nil, errors.New("素材缺少击杀前镜头")
			}
			args := []string{"-hide_banner", "-loglevel", "error", "-y", "-ss", fmt.Sprintf("%.3f", offset), "-i", raw, "-t", strconv.FormatFloat(wantedDuration, 'f', 3, 64), "-map", "0:v:0", "-map", "0:a:0?", "-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart", temp}
			if e := runCommand(a.ctx, 90*time.Second, c.FFmpeg, args...); e != nil {
				os.Remove(temp)
				return nil, e
			}
			var e error
			duration, e = probe(a.ctx, c.FFprobe, temp)
			if e != nil {
				os.Remove(temp)
				return nil, e
			}
			if duration < wantedDuration-0.05 || duration > wantedDuration+0.08 {
				os.Remove(temp)
				return nil, fmt.Errorf("片段时长不合格: %.3fs", duration)
			}
		}
		hash, size, e := fileHash(temp)
		if e != nil {
			return nil, e
		}
		if e = replaceFile(temp, path); e != nil {
			return nil, e
		}
		quality := "estimated"
		if j.Demo {
			quality = "demo"
		}
		cameraQuality := "sampled"
		if j.Demo {
			cameraQuality = "demo"
		}
		art := Artifact{Utility: event.Utility, CameraQuality: cameraQuality, CameraEvidence: cameraEvidence, ID: artifactID, EventID: event.ID, JobID: j.ID, Name: event.Name, Player: event.Player, Round: event.Round, Path: path, Size: size, SHA256: hash, Duration: duration, Quality: quality, Created: nowMS(), Demo: j.Demo}
		artifacts = append(artifacts, art)
	}
	return artifacts, nil
}
func runCommand(parent context.Context, timeout time.Duration, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	b, e := cmd.CombinedOutput()
	if e != nil {
		message := string(b)
		if len(message) > 2000 {
			message = message[len(message)-2000:]
		}
		return fmt.Errorf("%s: %w %s", filepath.Base(name), e, message)
	}
	return nil
}
func probe(parent context.Context, ffprobe, path string) (float64, error) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	b, e := exec.CommandContext(ctx, ffprobe, "-v", "error", "-show_entries", "format=duration:stream=codec_type,codec_name", "-of", "json", path).Output()
	if e != nil {
		return 0, fmt.Errorf("ffprobe: %w", e)
	}
	var r struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			Type  string `json:"codec_type"`
			Codec string `json:"codec_name"`
		} `json:"streams"`
	}
	if e = json.Unmarshal(b, &r); e != nil {
		return 0, e
	}
	video := false
	for _, s := range r.Streams {
		if s.Type == "video" && s.Codec == "h264" {
			video = true
		}
	}
	if !video {
		return 0, errors.New("缺少 H.264 视频轨")
	}
	return strconv.ParseFloat(r.Format.Duration, 64)
}
func fileHash(path string) (string, int64, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", 0, e
	}
	defer f.Close()
	h := sha256.New()
	n, e := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, e
}
func (a *Service) preflight() map[string]any {
	a.mu.Lock()
	c := jsonCopy(a.s.Config)
	observed := a.observed
	fresh := nowMS()-a.observedAt < 1500
	a.mu.Unlock()
	result := map[string]any{"mode": c.Mode, "gsi_fresh": fresh, "observed": observed, "calibrated": c.CalibratedUntil > nowMS(), "strict_supported": false, "utility_tracking_configured": c.TrackingMode == "native" || c.TrackingURL != "", "tracking_mode": c.TrackingMode}
	for key, exe := range map[string]string{"ffmpeg": c.FFmpeg, "ffprobe": c.FFprobe} {
		p, e := exec.LookPath(exe)
		if e != nil {
			result[key] = e.Error()
		} else {
			result[key] = p
		}
	}
	o, e := openOBS(c)
	if e != nil {
		result["obs"] = e.Error()
	} else {
		defer o.close()
		v, e := o.call("GetVersion", nil)
		if e != nil {
			result["obs"] = e.Error()
		} else {
			result["obs"] = v
		}
	}
	return result
}
func (a *Service) play(artifactID string) error {
	a.playbackMu.Lock()
	defer a.playbackMu.Unlock()
	a.mu.Lock()
	c := a.s.Config
	var selected *Artifact
	for _, v := range a.s.Artifacts {
		if v.ID == artifactID {
			v2 := v
			selected = &v2
		}
	}
	busy := a.playback != ""
	a.mu.Unlock()
	if busy {
		return errors.New("当前已有回放播出，请先返回直播")
	}
	if selected == nil {
		return errors.New("素材不存在")
	}
	hash, size, e := fileHash(selected.Path)
	if e != nil {
		return e
	}
	if hash != selected.SHA256 || size != selected.Size {
		return errors.New("播出前素材完整性校验失败")
	}
	if c.Mode == "demo" {
		return errors.New("演示模式仅预览；切换真实模式并配置 OBS 后可手动播出")
	}
	o, e := openOBS(c)
	if e != nil {
		return e
	}
	defer o.close()
	scene, e := o.call("GetCurrentProgramScene", nil)
	if e != nil {
		return e
	}
	live := stringField(scene, "currentProgramSceneName")
	if live == c.ReplayScene {
		return errors.New("当前已处于回放场景，请先切回直播")
	}
	items, e := o.call("GetSceneItemList", map[string]any{"sceneName": c.ReplayScene})
	if e != nil {
		return e
	}
	found := false
	if list, ok := items["sceneItems"].([]any); ok {
		for _, v := range list {
			if m, ok := v.(map[string]any); ok && m["sourceName"] == c.ReplayInput && m["sceneItemEnabled"] == true {
				found = true
			}
		}
	}
	if !found {
		return errors.New("Replay 场景中没有启用的指定媒体源")
	}
	if _, e = o.call("SetInputSettings", map[string]any{"inputName": c.ReplayInput, "inputSettings": map[string]any{"local_file": selected.Path, "is_local_file": true, "looping": false, "restart_on_activate": true}, "overlay": true}); e != nil {
		return e
	}
	if _, e = o.call("SetCurrentProgramScene", map[string]any{"sceneName": c.ReplayScene}); e != nil {
		return e
	}
	if _, e = o.call("TriggerMediaInputAction", map[string]any{"inputName": c.ReplayInput, "mediaAction": "OBS_WEBSOCKET_MEDIA_INPUT_ACTION_RESTART"}); e != nil {
		o.call("SetCurrentProgramScene", map[string]any{"sceneName": live})
		return e
	}
	a.mu.Lock()
	a.playback = artifactID
	a.liveScene = live
	a.logLocked("info", "导播手动播出："+selected.Name)
	a.mu.Unlock()
	a.wg.Add(1)
	go func() { defer a.wg.Done(); a.watchPlayback(artifactID, c) }()
	return nil
}
func (a *Service) watchPlayback(artifactID string, c Config) {
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if a.waitUntil(nowMS()+200) != nil {
			return
		}
		a.mu.Lock()
		owned := a.playback == artifactID
		a.mu.Unlock()
		if !owned {
			return
		}
		o, e := openOBS(c)
		if e != nil {
			continue
		}
		scene, e := o.call("GetCurrentProgramScene", nil)
		if e == nil && stringField(scene, "currentProgramSceneName") != c.ReplayScene {
			o.close()
			a.mu.Lock()
			a.playback = ""
			a.liveScene = ""
			a.mu.Unlock()
			return
		}
		m, e := o.call("GetMediaInputStatus", map[string]any{"inputName": c.ReplayInput})
		o.close()
		if e == nil && strings.HasSuffix(stringField(m, "mediaState"), "_ENDED") {
			a.returnLive()
			return
		}
	}
	a.mu.Lock()
	a.logLocked("warn", "未确认播放结束，请检查 OBS 或点击返回直播")
	a.mu.Unlock()
}
func (a *Service) returnLive() error {
	a.playbackMu.Lock()
	defer a.playbackMu.Unlock()
	a.mu.Lock()
	c := a.s.Config
	live := a.liveScene
	a.mu.Unlock()
	if live == "" {
		return errors.New("没有本程序持有的回放会话")
	}
	o, e := openOBS(c)
	if e != nil {
		return e
	}
	defer o.close()
	current, e := o.call("GetCurrentProgramScene", nil)
	if e != nil {
		return e
	}
	if stringField(current, "currentProgramSceneName") == c.ReplayScene {
		if _, e = o.call("SetCurrentProgramScene", map[string]any{"sceneName": live}); e != nil {
			return e
		}
	}
	a.mu.Lock()
	a.playback = ""
	a.liveScene = ""
	a.logLocked("info", "回放会话结束")
	a.mu.Unlock()
	return nil
}
