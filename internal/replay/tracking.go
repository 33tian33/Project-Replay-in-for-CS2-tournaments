package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

func validateUtility(u *Utility) error {
	if u == nil {
		return nil
	}
	switch u.Kind {
	case "hegrenade", "molotov", "incgrenade", "flashbang", "smokegrenade", "decoy":
	default:
		return errors.New("未知道具类型")
	}
	if err := validateTrack(u.Track); err != nil {
		return err
	}
	if u.EffectCamera != nil {
		for _, v := range u.EffectCamera {
			if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 100000 {
				return errors.New("作用区域镜头坐标无效")
			}
		}
	}
	// Missing linkage is a valid candidate, but cannot be scheduled.
	if u.ID != "" && !validID(u.ID) {
		return errors.New("道具实例 ID 无效")
	}
	if len(u.Evidence) > 4000 {
		return errors.New("道具关联证据过长")
	}
	return nil
}
func resolvedUtility(u *Utility) bool {
	return u == nil || (validateUtility(u) == nil && u.ID != "" && strings.TrimSpace(u.Evidence) != "")
}
func sameTarget(a, b Event) bool {
	if a.Player != b.Player {
		return false
	}
	if a.Utility == nil || b.Utility == nil {
		return a.Utility == nil && b.Utility == nil
	}
	return resolvedUtility(a.Utility) && resolvedUtility(b.Utility) && a.Utility.ID == b.Utility.ID && a.Utility.Kind == b.Utility.Kind && a.Match == b.Match && a.Map == b.Map && a.Epoch == b.Epoch
}
func validateTrackingURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("道具镜头适配器地址必须为 http(s)://主机:端口")
	}
	return nil
}

type cameraMonitor interface {
	Check() error
	Close()
	Evidence() []trackingSample
}

type playerCamera struct {
	service  *Service
	job      Job
	switchAt int64
}

func (m *playerCamera) Check() error {
	a := m.service
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.checkRoundJobLocked(m.job); err != nil {
		return err
	}
	if nowMS() < m.job.Start && a.s.Config.Epoch == m.job.Epoch {
		return nil
	}
	if a.observedAt <= m.switchAt || a.observed != m.job.Player || nowMS()-a.observedAt >= 1500 || a.s.Config.Epoch != m.job.Epoch {
		return errors.New("WRONG_TARGET：录制期间目标变化、GSI 断流或时间线失效")
	}
	return nil
}
func (*playerCamera) Close()                     {}
func (*playerCamera) Evidence() []trackingSample { return nil }

// prepareCamera does not issue player commands for utility kills. The bridge owns
// feed-local entity resolution, following the projectile and holding its effect.
func (a *Service) prepareCamera(j Job, c Config) (cameraMonitor, error) {
	if err := validateUtility(j.Utility); err != nil {
		return nil, err
	}
	if !resolvedUtility(j.Utility) {
		return nil, errors.New("TRACKING_UNRESOLVED：缺少道具实例或关联证据")
	}
	if j.Utility != nil {
		if c.TrackingMode == "native" {
			return a.prepareNativeCamera(j, c)
		}
		if c.TrackingURL == "" {
			return nil, errors.New("TRACKING_UNAVAILABLE：未配置道具镜头适配器")
		}
		m := &utilityCamera{service: a, job: j, config: c}
		// Release even after a lost acknowledgement: prepare may already have applied.
		if err := m.call("PUT", j, nil); err != nil {
			m.Close()
			return nil, err
		}
		for nowMS() < j.Start-150 {
			if err := m.Check(); err == nil && nowMS() < j.Start-150 {
				return m, nil
			}
			if err := a.waitUntil(nowMS() + 40); err != nil {
				m.Close()
				return nil, err
			}
		}
		m.Close()
		return nil, errors.New("WRONG_TARGET：准备期限内未确认道具镜头")
	}
	a.mu.Lock()
	command, commandErr := a.playerCommandLocked(j, c)
	fresh := a.observedAt > nowMS()-2000 && a.s.Config.Epoch == j.Epoch
	a.mu.Unlock()
	if commandErr != nil {
		return nil, commandErr
	}
	if !fresh {
		return nil, errors.New("B 路 GSI 不新鲜或会话失效")
	}
	if err := netCommand(c.NetCon, "spec_autodirector 0; spec_mode 1"); err != nil {
		return nil, err
	}
	switchAt := nowMS()
	if err := netCommand(c.NetCon, command+"; "+a.recordingHUDCommand()); err != nil {
		return nil, err
	}
	return &playerCamera{service: a, job: j, switchAt: switchAt}, nil
}

type trackingSample struct {
	JobID          string `json:"job_id"`
	Epoch          int    `json:"epoch"`
	UtilityID      string `json:"utility_id"`
	Phase          string `json:"phase"`       // projectile -> effect; starting in effect is valid.
	ObservedAt     int64  `json:"observed_at"` // worker wall clock, milliseconds
	CameraVerified bool   `json:"camera_verified"`
	Evidence       string `json:"evidence"`
}
type utilityCamera struct {
	service   *Service
	job       Job
	config    Config
	phase     string
	samples   []trackingSample
	closeOnce sync.Once
}

func (m *utilityCamera) call(method string, body, result any) error {
	if err := validateTrackingURL(m.config.TrackingURL); err != nil {
		return err
	}
	if !validID(m.job.ID) {
		return errors.New("道具追踪任务 ID 无效")
	}
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	ctx := m.service.ctx
	timeout := 500 * time.Millisecond
	if method == "PUT" {
		timeout = time.Until(time.UnixMilli(m.job.Start - 150))
		if timeout <= 0 {
			return errors.New("MISSED_WINDOW：已错过道具准备期限")
		}
	}
	// Cleanup must survive cancellation of capture.
	if method == "DELETE" {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(m.config.TrackingURL, "/")+"/v1/tracking/"+m.job.ID, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	r, err := httpClient.Do(req)
	if err != nil {
		return errors.New("TRACKING_UNAVAILABLE：道具镜头适配器请求失败")
	}
	defer r.Body.Close()
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return fmt.Errorf("TRACKING_UNAVAILABLE：道具镜头适配器 HTTP %d", r.StatusCode)
	}
	if result != nil {
		return json.NewDecoder(io.LimitReader(r.Body, 16384)).Decode(result)
	}
	return nil
}
func (m *utilityCamera) Check() error {
	m.service.mu.Lock()
	valid := m.service.s.Config.Epoch == m.job.Epoch
	m.service.mu.Unlock()
	if !valid {
		return errors.New("WRONG_TARGET：时间线失效")
	}
	var s trackingSample
	if err := m.call("GET", nil, &s); err != nil {
		return err
	}
	age := nowMS() - s.ObservedAt
	if s.JobID != m.job.ID || s.Epoch != m.job.Epoch || s.UtilityID != m.job.Utility.ID || !s.CameraVerified || strings.TrimSpace(s.Evidence) == "" || age < 0 || age > 500 || (s.Phase != "projectile" && s.Phase != "effect") || (m.phase == "effect" && s.Phase != "effect") {
		return errors.New("WRONG_TARGET：道具镜头身份、阶段或新鲜验证证据无效")
	}
	// Retain the first sample of each phase and the latest sample, bounded to 4.
	if len(m.samples) == 0 || m.phase != s.Phase {
		m.samples = append(m.samples, s, s)
	} else {
		m.samples[len(m.samples)-1] = s
	}
	m.phase = s.Phase
	return nil
}
func (m *utilityCamera) Evidence() []trackingSample {
	return append([]trackingSample(nil), m.samples...)
}
func (m *utilityCamera) Close() {
	m.closeOnce.Do(func() {
		if err := m.call("DELETE", nil, nil); err != nil {
			m.service.mu.Lock()
			m.service.s.Config.Paused = true
			m.service.logLocked("warn", "道具镜头释放未确认；已暂停采集，请检查适配器")
			m.service.mu.Unlock()
		}
	})
}
