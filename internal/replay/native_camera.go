package replay

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TrackPoint is a sample in the recording worker's wall-clock domain.
// A source adapter maps game ticks into this domain before submitting the event.
type TrackPoint struct {
	Time     int64      `json:"time"`
	Position [3]float64 `json:"position"`
	Phase    string     `json:"phase"`
}

func validateTrack(points []TrackPoint) error {
	if len(points) == 0 {
		return nil
	}
	if len(points) < 2 || len(points) > 2048 {
		return errors.New("轨迹需要 2–2048 个采样点")
	}
	effect := false
	for i, p := range points {
		for _, v := range p.Position {
			if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 100000 {
				return errors.New("轨迹坐标无效")
			}
		}
		if p.Phase != "projectile" && p.Phase != "effect" {
			return errors.New("轨迹阶段无效")
		}
		if effect && p.Phase != "effect" {
			return errors.New("作用区域不能回退为飞行道具")
		}
		effect = p.Phase == "effect"
		if i > 0 && (p.Time <= points[i-1].Time || (p.Time-points[i-1].Time > 250 && p.Phase == "projectile")) {
			return errors.New("轨迹时间倒退或飞行采样间隙超过 250ms")
		}
	}
	if points[len(points)-1].Time-points[0].Time > 60000 {
		return errors.New("轨迹超过 60 秒")
	}
	return nil
}
func nativeTrackReady(e Event, c Config) bool {
	if e.Utility == nil || len(e.Utility.Track) < 2 || validateTrack(e.Utility.Track) != nil {
		return false
	}
	start, end := bounds(e, c)
	return e.Utility.Track[0].Time <= start && e.Utility.Track[len(e.Utility.Track)-1].Time >= end
}
func shiftUtility(u *Utility, offset int64) {
	if u == nil {
		return
	}
	for i := range u.Track {
		u.Track[i].Time += offset
	}
}

// Reconstruct the task trajectory from its events; never trust a separate,
// possibly inconsistent job-level trajectory received from another node.
func mergedUtility(events []Event) (*Utility, error) {
	if len(events) == 0 || events[0].Utility == nil {
		return nil, nil
	}
	u := jsonCopy(*events[0].Utility)
	u.Track = nil
	byTime := map[int64]TrackPoint{}
	for _, e := range events {
		if !sameTarget(events[0], e) {
			return nil, errors.New("合并事件的道具实例不同")
		}
		if (u.EffectCamera == nil) != (e.Utility.EffectCamera == nil) || (u.EffectCamera != nil && *u.EffectCamera != *e.Utility.EffectCamera) {
			return nil, errors.New("同一火区的镜头位置不同")
		}
		for _, p := range e.Utility.Track {
			if old, exists := byTime[p.Time]; exists && old != p {
				return nil, errors.New("同一道具轨迹采样冲突")
			}
			byTime[p.Time] = p
		}
	}
	for _, p := range byTime {
		u.Track = append(u.Track, p)
	}
	sort.Slice(u.Track, func(i, j int) bool { return u.Track[i].Time < u.Track[j].Time })
	return &u, validateTrack(u.Track)
}

// Sample linearly interpolates measured positions; it never extrapolates flight.
func sampleTrack(points []TrackPoint, at int64) ([3]float64, [3]float64, string, error) {
	var zero [3]float64
	if len(points) < 2 || at < points[0].Time || at > points[len(points)-1].Time {
		return zero, zero, "", errors.New("TRACKING_LOST：时间超出已知轨迹")
	}
	i := 1
	for i < len(points)-1 && points[i].Time < at {
		i++
	}
	a, b := points[i-1], points[i]
	phase := a.Phase
	if at == b.Time {
		phase = b.Phase
	}
	pos := a.Position
	f := float64(at-a.Time) / float64(b.Time-a.Time)
	dir := zero
	for k := range pos {
		pos[k] += f * (b.Position[k] - a.Position[k])
		dir[k] = b.Position[k] - a.Position[k]
	}
	return pos, dir, phase, nil
}

type cameraPose struct {
	Pos        [3]float64
	Pitch, Yaw float64
}

func followPose(pos, dir [3]float64, previous *cameraPose) cameraPose {
	length := math.Sqrt(dir[0]*dir[0] + dir[1]*dir[1] + dir[2]*dir[2])
	if length < .001 {
		if previous != nil {
			return *previous
		}
		dir = [3]float64{1, 0, 0}
		length = 1
	}
	// Keep the camera above the projectile; the offset avoids the projectile mesh.
	xy := math.Hypot(dir[0], dir[1])
	if xy < .001 {
		dir[0] = 1
		dir[1] = 0
		xy = 1
	}
	out := cameraPose{Pos: [3]float64{pos[0] - 96*dir[0]/xy, pos[1] - 96*dir[1]/xy, pos[2] + 48}}
	d := [3]float64{pos[0] - out.Pos[0], pos[1] - out.Pos[1], pos[2] - out.Pos[2]}
	out.Pitch = -math.Atan2(d[2], math.Hypot(d[0], d[1])) * 180 / math.Pi
	out.Yaw = math.Atan2(d[1], d[0]) * 180 / math.Pi
	return out
}

var specPosition = regexp.MustCompile(`(?m)^spec_goto\s+([-+0-9.eE]+)\s+([-+0-9.eE]+)\s+([-+0-9.eE]+)\s+([-+0-9.eE]+)\s+([-+0-9.eE]+)\s*$`)

func parsePose(s string) (cameraPose, error) {
	m := specPosition.FindStringSubmatch(strings.ReplaceAll(s, "\r", ""))
	if m == nil {
		return cameraPose{}, errors.New("CAMERA_UNVERIFIED：spec_pos 没有返回实际镜头；确认观战模式和命令权限")
	}
	v := [5]float64{}
	for i := range v {
		n, e := strconv.ParseFloat(m[i+1], 64)
		if e != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return cameraPose{}, errors.New("镜头反馈无效")
		}
		v[i] = n
	}
	return cameraPose{[3]float64{v[0], v[1], v[2]}, v[3], v[4]}, nil
}
func poseMatches(a, b cameraPose) bool {
	d := 0.0
	for i := range a.Pos {
		d += (a.Pos[i] - b.Pos[i]) * (a.Pos[i] - b.Pos[i])
	}
	return d <= 16 && math.Abs(math.Remainder(a.Yaw-b.Yaw, 360)) <= 2 && math.Abs(a.Pitch-b.Pitch) <= 2
}

// consoleSession keeps framing and readback on a single connection.
type consoleSession struct {
	conn   net.Conn
	reader *bufio.Reader
}

func openConsole(address string) (*consoleSession, error) {
	c, e := net.DialTimeout("tcp", address, time.Second)
	if e != nil {
		return nil, e
	}
	return &consoleSession{c, bufio.NewReaderSize(c, 65536)}, nil
}
func (n *consoleSession) query(command string) (string, error) {
	return n.queryWithTimeout(command, 400*time.Millisecond)
}
func (n *consoleSession) queryWithTimeout(command string, timeout time.Duration) (string, error) {
	if strings.ContainsAny(command, "\r\n") {
		return "", errors.New("控制台命令含换行")
	}
	marker := "REPLAY_" + id()
	n.conn.SetDeadline(time.Now().Add(timeout))
	if _, e := fmt.Fprintf(n.conn, "%s\necho %s\n", command, marker); e != nil {
		return "", e
	}
	var b strings.Builder
	for b.Len() < 131072 {
		line, e := n.reader.ReadString('\n')
		if e != nil {
			return "", e
		}
		if strings.TrimSpace(line) == marker {
			return b.String(), nil
		}
		b.WriteString(line)
	}
	return "", errors.New("控制台反馈过长")
}

type nativeCamera struct {
	service   *Service
	job       Job
	console   *consoleSession
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	mu        sync.Mutex
	err       error
	sample    trackingSample
	samples   []trackingSample
	lastPose  *cameraPose
	closeOnce sync.Once
}

func (a *Service) prepareNativeCamera(j Job, c Config) (cameraMonitor, error) {
	if len(j.Events) == 0 || j.Utility == nil || validateTrack(j.Utility.Track) != nil || len(j.Utility.Track) < 2 || j.Utility.Track[0].Time > j.Start || j.Utility.Track[len(j.Utility.Track)-1].Time < j.End {
		return nil, errors.New("TRACKING_UNRESOLVED：缺少覆盖完整录制窗口的道具轨迹")
	}
	n, e := openConsole(c.NetCon)
	if e != nil {
		return nil, e
	}
	ctx, cancel := context.WithCancel(a.ctx)
	m := &nativeCamera{service: a, job: j, console: n, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	// No sv_cheats changes: a server that denies readback remains unsupported.
	if _, e = n.query("spec_autodirector 0; spec_mode 4; cl_obs_interp_enable 0"); e != nil {
		cancel()
		n.conn.Close()
		return nil, e
	}
	go m.run()
	for nowMS() < j.Start-150 {
		if m.Check() == nil {
			return m, nil
		}
		if e = a.waitUntil(nowMS() + 20); e != nil {
			m.Close()
			return nil, e
		}
	}
	e = m.Check()
	m.Close()
	if e == nil {
		e = errors.New("镜头确认晚于准备期限")
	}
	return nil, e
}
func (m *nativeCamera) run() {
	defer close(m.done)
	ticker := time.NewTicker(time.Second / 30)
	defer ticker.Stop()
	for {
		if m.ctx.Err() != nil {
			return
		}
		m.service.mu.Lock()
		valid := m.service.s.Config.Epoch == m.job.Epoch && m.service.s.Config.CalibratedUntil > nowMS()
		m.service.mu.Unlock()
		var err error
		if !valid {
			err = errors.New("WRONG_TARGET：时间线失效")
		} else {
			err = m.update()
		}
		m.mu.Lock()
		m.err = err
		m.mu.Unlock()
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (m *nativeCamera) update() error {
	points := m.job.Utility.Track
	at := max(nowMS(), m.job.Start)
	at = min(at, m.job.End)
	pos, dir, phase, e := sampleTrack(points, at)
	if e != nil {
		return e
	}
	pose := followPose(pos, dir, m.lastPose)
	if phase == "effect" && m.lastPose == nil {
		// Use a measured, occupiable viewpoint when provided by the source.
		// A generic raised orbit can cut through ceilings in indoor fire zones.
		if eye := m.job.Utility.EffectCamera; eye != nil {
			dx, dy, dz := pos[0]-eye[0], pos[1]-eye[1], pos[2]-eye[2]
			pose = cameraPose{Pos: *eye, Pitch: -math.Atan2(dz, math.Hypot(dx, dy)) * 180 / math.Pi, Yaw: math.Atan2(dy, dx) * 180 / math.Pi}
		}
	}
	if phase == "effect" && m.lastPose != nil {
		pose = *m.lastPose
	}
	_, e = m.console.query(fmt.Sprintf("spec_goto %.4f %.4f %.4f %.4f %.4f", pose.Pos[0], pose.Pos[1], pose.Pos[2], pose.Pitch, pose.Yaw))
	if e != nil {
		return e
	}
	// Rendering applies orientation on the next game frame, not on console echo.
	select {
	case <-m.ctx.Done():
		return m.ctx.Err()
	case <-time.After(18 * time.Millisecond):
	}
	response, e := m.console.query("spec_pos")
	if e != nil {
		return e
	}
	actual, e := parsePose(response)
	if e != nil {
		return e
	}
	if !poseMatches(pose, actual) {
		return errors.New("WRONG_TARGET：实际相机未到达道具跟随位置")
	}
	m.lastPose = &pose
	sample := trackingSample{JobID: m.job.ID, Epoch: m.job.Epoch, UtilityID: m.job.Utility.ID, Phase: phase, ObservedAt: nowMS(), CameraVerified: true, Evidence: strings.TrimSpace(response)}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.samples) == 0 || m.sample.Phase != phase {
		m.samples = append(m.samples, sample, sample)
	} else {
		m.samples[len(m.samples)-1] = sample
	}
	m.sample = sample
	return nil
}
func (m *nativeCamera) Check() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	if m.sample.ObservedAt == 0 || nowMS()-m.sample.ObservedAt > 500 {
		return errors.New("CAMERA_UNVERIFIED：原生镜头反馈尚未就绪或过期")
	}
	return nil
}
func (m *nativeCamera) Evidence() []trackingSample {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]trackingSample(nil), m.samples...)
}
func (m *nativeCamera) Close() {
	m.closeOnce.Do(func() { m.cancel(); <-m.done; m.console.conn.Close() })
}
