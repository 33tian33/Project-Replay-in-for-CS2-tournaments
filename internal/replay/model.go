package replay

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"projectreplay/internal/relay"
	"strings"
	"time"
)

const Version = "0.2.6"

func nowMS() int64 { return time.Now().UnixMilli() }
func id() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}

type Config struct {
	ConnectionMode string `json:"connection_mode"`
	RelayURL       string `json:"relay_url"`
	RelayDevice    string `json:"relay_device"`
	RelayGroup     string `json:"relay_group"`
	RelayName      string `json:"relay_name"`
	RelayAutoPair  bool   `json:"relay_auto_pair"`
	RelayPair      string `json:"relay_pair,omitempty"`

	Teams           TeamSettings   `json:"teams"`
	TestMode        bool           `json:"test_mode"`
	AutoSendGOTV    bool           `json:"auto_send_gotv"`
	WorkflowVersion int            `json:"workflow_version"`
	AutoCapture     bool           `json:"auto_capture"`
	EventSource     string         `json:"event_source,omitempty"`
	TrackingMode    string         `json:"tracking_mode,omitempty"`
	TrackingURL     string         `json:"tracking_url,omitempty"`
	Mode            string         `json:"mode"`
	Match           string         `json:"match"`
	Map             string         `json:"map"`
	Epoch           int            `json:"epoch"`
	Paused          bool           `json:"paused"`
	Strict          bool           `json:"strict"`
	Delta           float64        `json:"delta"`
	Uncertainty     float64        `json:"uncertainty"`
	CalibratedUntil int64          `json:"calibrated_until"`
	Guard           float64        `json:"guard"`
	Setup           float64        `json:"setup"`
	Transition      float64        `json:"transition"`
	WorkerURL       string         `json:"worker_url"`
	OBSURL          string         `json:"obs_url"`
	OBSPassword     string         `json:"obs_password,omitempty"`
	NetCon          string         `json:"netcon"`
	GOTVPassword    string         `json:"gotv_password,omitempty"`
	GOTV            string         `json:"gotv"`
	FFmpeg          string         `json:"ffmpeg"`
	FFprobe         string         `json:"ffprobe"`
	SessionLock     string         `json:"session_lock"`
	ReplayScene     string         `json:"replay_scene"`
	ReplayInput     string         `json:"replay_input"`
	Mappings        map[string]int `json:"mappings"`
}

func defaults() Config {
	return Config{RelayDevice: relay.ID(), WorkflowVersion: 1, Mode: "live", Match: "训练赛", Map: "", Epoch: 1, Paused: false, AutoCapture: true, Strict: false, Delta: 8, Uncertainty: .15, Guard: .2, Setup: .2, Transition: 0, OBSURL: "ws://127.0.0.1:4455", NetCon: "127.0.0.1:2121", FFmpeg: "ffmpeg", FFprobe: "ffprobe", ReplayScene: "Replay", ReplayInput: "Replay Media", Mappings: map[string]int{}}
}
func (c Config) validate() error {
	if c.ConnectionMode != "" && c.ConnectionMode != "lan" && c.ConnectionMode != "relay" {
		return errors.New("连接模式须为局域网或云中继")
	}
	if c.ConnectionMode == "relay" {
		if err := relay.ValidateURL(c.RelayURL); err != nil {
			return err
		}
		if !relay.GroupPattern.MatchString(c.RelayGroup) {
			return errors.New("群组码必须是 4 位数字，允许前导零")
		}
		if !relay.IDPattern.MatchString(c.RelayDevice) {
			return errors.New("中继设备标识无效")
		}
		if len(c.RelayName) > 80 {
			return errors.New("设备名称过长")
		}
	}

	if c.GOTV != "" {
		if _, _, err := parseGOTV(c.GOTV); err != nil {
			return err
		}
	}
	if err := validateGOTVPassword(c.GOTVPassword); err != nil {
		return err
	}
	if err := c.Teams.validate(); err != nil {
		return err
	}
	if c.EventSource != "" && c.EventSource != "gsi" && c.EventSource != "parser" {
		return errors.New("事件源必须为 gsi 或 parser")
	}
	if c.TrackingMode != "" && c.TrackingMode != "bridge" && c.TrackingMode != "native" {
		return errors.New("追踪后端必须为 bridge 或 native")
	}
	if err := validateTrackingURL(c.TrackingURL); err != nil {
		return err
	}
	if c.Mode != "demo" && c.Mode != "live" {
		return errors.New("模式必须为 demo 或 live")
	}
	if c.Match == "" {
		return errors.New("比赛不能为空")
	}
	if c.Epoch < 1 || c.Delta < 0 || c.Delta > 300 || c.Uncertainty < 0 || c.Uncertainty > 5 || c.Guard < .05 || c.Guard > 5 || c.Setup < 0 || c.Setup > 20 || c.Transition < 0 || c.Transition > 20 {
		return errors.New("时序参数超出范围")
	}
	for _, v := range []float64{c.Delta, c.Uncertainty, c.Guard, c.Setup, c.Transition} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("无效数值")
		}
	}
	for s, n := range c.Mappings {
		if s == "" || n < 1 || n > 128 {
			return errors.New("身份映射必须是非空 SteamID 与 1–128 命令槽位")
		}
	}
	return nil
}

// Utility identifies one throw across feeds; ID is not a feed-local entity index.
type Utility struct {
	EffectCamera *[3]float64  `json:"effect_camera,omitempty"`
	Track        []TrackPoint `json:"track,omitempty"`
	Kind         string       `json:"kind"`
	ID           string       `json:"id"`
	Evidence     string       `json:"evidence"`
}

type Event struct {
	ClockAnchor  *RoundClock `json:"clock_anchor,omitempty"`
	ClockOffset  float64     `json:"clock_offset,omitempty"`
	LiveDuration float64     `json:"live_duration,omitempty"`
	Clutch       int         `json:"clutch,omitempty"`
	RoundTiming  bool        `json:"round_timing,omitempty"`
	Clock        *RoundClock `json:"clock,omitempty"`
	Utility      *Utility    `json:"utility,omitempty"`
	ID           string      `json:"id"`
	Match        string      `json:"match"`
	Map          string      `json:"map"`
	Epoch        int         `json:"epoch"`
	Player       string      `json:"player"`
	Name         string      `json:"name"`
	Victim       string      `json:"victim,omitempty"`
	Time         int64       `json:"time"`
	Tick         *int64      `json:"tick"`
	TickDomain   string      `json:"tick_domain,omitempty"`
	Quality      string      `json:"quality"`
	Uncertainty  float64     `json:"uncertainty"`
	Evidence     string      `json:"evidence,omitempty"`
	Round        int         `json:"round"`
	Kills        int         `json:"kills"`
	GroupCount   int         `json:"group_count"`
	Missed       bool        `json:"missed"`
	Pin          bool        `json:"pin"`
	Status       string      `json:"status"`
	Reason       string      `json:"reason"`
	JobID        string      `json:"job_id,omitempty"`
}
type Job struct {
	Utility   *Utility   `json:"utility,omitempty"`
	ID        string     `json:"id"`
	Epoch     int        `json:"epoch"`
	Match     string     `json:"match"`
	Map       string     `json:"map"`
	Player    string     `json:"player"`
	Slot      int        `json:"slot"`
	Events    []Event    `json:"events"`
	Start     int64      `json:"start"`
	End       int64      `json:"end"`
	Status    string     `json:"status"`
	Error     string     `json:"error"`
	Demo      bool       `json:"demo"`
	Artifacts []Artifact `json:"artifacts"`
}
type Artifact struct {
	Utility        *Utility         `json:"utility,omitempty"`
	CameraQuality  string           `json:"camera_quality,omitempty"`
	CameraEvidence []trackingSample `json:"camera_evidence,omitempty"`
	ID             string           `json:"id"`
	EventID        string           `json:"event_id"`
	JobID          string           `json:"job_id"`
	Name           string           `json:"name"`
	Player         string           `json:"player"`
	Round          int              `json:"round"`
	Path           string           `json:"-"`
	Size           int64            `json:"size"`
	SHA256         string           `json:"sha256"`
	Duration       float64          `json:"duration"`
	Quality        string           `json:"quality"`
	Created        int64            `json:"created"`
	Demo           bool             `json:"demo"`
}
type Log struct {
	Time    int64  `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
}
type State struct {
	HUDPackage        HUDPackage     `json:"hud_package"`
	HUD               HUDSettings    `json:"hud"`
	HUDActiveSource   string         `json:"hud_active_source"`
	TeamHUD           bool           `json:"team_hud"`
	Output            OutputSettings `json:"output"`
	HalfQueue         []string       `json:"half_queue"`
	FullQueue         []string       `json:"full_queue"`
	HighlightsVersion int            `json:"highlights_version"`
	Config            Config         `json:"config"`
	Events            []Event        `json:"events"`
	Jobs              []Job          `json:"jobs"`
	Artifacts         []Artifact     `json:"artifacts"`
	Queue             []string       `json:"queue"`
	Logs              []Log          `json:"logs"`
}

// Disk artifacts include local paths, but network representations never disclose them.
type diskState struct {
	State State             `json:"state"`
	Paths map[string]string `json:"paths"`
}

func saveJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".state-*")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return replaceFile(tmp, path)
}
func validID(s string) bool {
	if len(s) < 1 || len(s) > 100 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
func terminal(s string) bool {
	return s == "READY" || s == "FAILED" || s == "CANCELLED" || s == "MISSED_WINDOW"
}
func labelErr(e error) string { return strings.TrimSpace(fmt.Sprint(e)) }
