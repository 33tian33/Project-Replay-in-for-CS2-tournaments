package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"projectreplay/internal/relay"
	"strconv"
	"strings"
	"time"
)

type remoteHTTPError struct {
	Status int
	Body   string
}

func (e *remoteHTTPError) Error() string { return fmt.Sprintf("节点 HTTP %d: %s", e.Status, e.Body) }

var httpClient = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}

func remoteURL(c Config, path string) (string, error) {
	if c.ConnectionMode == "relay" {
		if err := relay.ValidateURL(c.RelayURL); err != nil {
			return "", err
		}
		if c.RelayPair == "" {
			return "", errors.New("云中继尚未完成配对")
		}
		prefix := "/v1/forward"
		if strings.HasPrefix(path, "/api/media/") {
			return strings.TrimRight(c.RelayURL, "/") + "/v1/media/" + strings.TrimPrefix(path, "/api/media/"), nil
		}
		return strings.TrimRight(c.RelayURL, "/") + prefix + path, nil
	}
	u, e := url.Parse(c.WorkerURL)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("节点地址必须是 http(s)://主机:端口")
	}
	return strings.TrimRight(c.WorkerURL, "/") + path, nil
}
func remoteCall(ctx context.Context, c Config, method, path string, body, result any) error {
	target, e := remoteURL(c, path)
	if e != nil {
		return e
	}
	var data []byte
	if body != nil {
		data, e = json.Marshal(body)
		if e != nil {
			return e
		}
	}
	req, e := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(data))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	client := httpClient
	if c.ConnectionMode == "relay" {
		relay.Headers(req, c.RelayDevice, c.RelayGroup, c.RelayPair)
		client = relayControlHTTP
	}
	r, e := client.Do(req)
	if e != nil {
		return e
	}
	defer r.Body.Close()
	if r.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		return &remoteHTTPError{Status: r.StatusCode, Body: string(b)}
	}
	if result != nil {
		return json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(result)
	}
	return nil
}
func (a *Service) syncRemote() error {
	a.mu.Lock()
	c := a.s.Config
	a.mu.Unlock()
	var best int64 = 1 << 62
	var offset int64
	for i := 0; i < 5; i++ {
		start := nowMS()
		var r struct {
			Now int64 `json:"now"`
		}
		if e := remoteCall(a.ctx, c, "GET", "/api/time", nil, &r); e != nil {
			return e
		}
		end := nowMS()
		if end-start < best {
			best = end - start
			offset = r.Now - (start+end)/2
		}
	}
	var state struct {
		Role  string `json:"role"`
		State State  `json:"state"`
	}
	if e := remoteCall(a.ctx, c, "GET", "/api/state", nil, &state); e != nil {
		return e
	}
	if state.Role != "agent" {
		return errors.New("目标程序必须以 -role agent 启动")
	}
	if a.role == "director" && state.State.Config.Teams != c.Teams {
		if err := remoteCall(a.ctx, c, "POST", "/api/teams?agent=1", c.Teams, nil); err != nil {
			return err
		}
	}
	if state.State.Config.Match != c.Match || state.State.Config.Map != c.Map {
		return errors.New("两端比赛或地图不一致，请在节点使用相同比赛和地图")
	}
	a.mu.Lock()
	if a.s.Config.Epoch != c.Epoch || a.s.Config.WorkerURL != c.WorkerURL || a.s.Config.ConnectionMode != c.ConnectionMode || a.s.Config.RelayPair != c.RelayPair {
		a.mu.Unlock()
		return errors.New("同步期间本机会话已变化")
	}
	a.remoteEpoch = state.State.Config.Epoch
	a.remoteOffset = offset
	a.remoteUncertainty = best/2 + 1
	a.remoteAt = nowMS()
	a.logLocked("info", fmt.Sprintf("节点时钟已同步：偏移 %dms，往返 %dms；此项不替代游戏时间线校准", offset, best))
	a.mu.Unlock()
	return nil
}
func (a *Service) runRemote(j Job, c Config) {
	// Resolve the receiver's local generation automatically for every submission.
	if err := a.syncRemote(); err != nil {
		a.jobUpdate(j.ID, "FAILED", err.Error(), nil)
		return
	}
	a.mu.Lock()
	offset, uncertainty, at, epoch := a.remoteOffset, a.remoteUncertainty, a.remoteAt, a.remoteEpoch
	a.mu.Unlock()
	if nowMS()-at > 60000 || at == 0 {
		a.jobUpdate(j.ID, "FAILED", "节点时钟同步超过 60 秒，重新连接节点后再录制", nil)
		return
	}
	if float64(uncertainty)/1000+c.Uncertainty > c.Guard {
		a.jobUpdate(j.ID, "FAILED", "网络时钟误差加游戏时序误差超过保护量", nil)
		return
	}
	remote := jsonCopy(j)
	remote.Epoch = epoch
	remote.Start += offset
	remote.End += offset
	shiftUtility(remote.Utility, offset)
	for i := range remote.Events {
		remote.Events[i].Epoch = epoch
		remote.Events[i].Time += offset
		shiftUtility(remote.Events[i].Utility, offset)
		remote.Events[i].Uncertainty += float64(uncertainty) / 1000
	}
	var accepted Job
	e := remoteCall(a.ctx, c, "POST", "/api/jobs", remote, &accepted)
	if e != nil { // Query the same ID after uncertain submission; never create a replacement recording.
		var rejected *remoteHTTPError
		if errors.As(e, &rejected) && rejected.Status >= 400 && rejected.Status < 500 {
			a.jobUpdate(j.ID, "FAILED", e.Error(), nil)
			return
		}
		if q := remoteCall(a.ctx, c, "GET", "/api/jobs/"+j.ID, nil, &accepted); q != nil {
			a.jobUpdate(j.ID, "FAILED", "提交结果未知；暂停采集，请在节点对账："+e.Error(), nil)
			a.mu.Lock()
			a.s.Config.Paused = true
			a.saveLocked()
			a.mu.Unlock()
			return
		}
	}
	until := time.Now().Add(time.Duration(max(j.End-nowMS(), 0))*time.Millisecond + 120*time.Second)
	for time.Now().Before(until) {
		if e = a.waitUntil(nowMS() + 300); e != nil {
			return
		}
		if e = remoteCall(a.ctx, c, "GET", "/api/jobs/"+j.ID, nil, &accepted); e != nil {
			continue
		}
		if accepted.Status == "READY" {
			a.jobUpdate(j.ID, "TRANSFERRING", "下载节点素材并校验", nil)
			arts := []Artifact{}
			for _, art := range accepted.Artifacts {
				if e = a.download(c, &art); e != nil {
					a.jobUpdate(j.ID, "FAILED", e.Error(), nil)
					return
				}
				arts = append(arts, art)
			}
			a.jobUpdate(j.ID, "READY", "节点素材已完整回传", arts)
			return
		}
		if terminal(accepted.Status) {
			a.jobUpdate(j.ID, accepted.Status, accepted.Error, nil)
			return
		}
	}
	a.jobUpdate(j.ID, "FAILED", "节点任务反馈超时；采集已暂停，请在节点核对任务", nil)
	a.mu.Lock()
	a.s.Config.Paused = true
	a.saveLocked()
	a.mu.Unlock()
}
func (a *Service) download(c Config, art *Artifact) error {
	if !validID(art.ID) || art.Size <= 0 || art.Size > 1<<30 || len(art.SHA256) != 64 {
		return errors.New("节点素材元数据无效")
	}
	path := filepath.Join(a.dir, "media", art.ID+".mp4")
	part := path + ".part"
	target, e := remoteURL(c, "/api/media/"+art.ID)
	if e != nil {
		return e
	}
	transferID := ""
	for attempt := 0; attempt < 3; attempt++ {
		var offset int64
		if st, e := os.Stat(part); e == nil {
			offset = st.Size()
		}
		if offset > art.Size {
			os.Remove(part)
			offset = 0
		}
		if offset < art.Size {
			timeout := 45 * time.Second
			if c.ConnectionMode == "relay" {
				timeout = 6 * time.Minute
			}
			ctx, cancel := context.WithTimeout(a.ctx, timeout)
			req, e := http.NewRequestWithContext(ctx, "GET", target, nil)
			if e != nil {
				cancel()
				return e
			}
			if offset > 0 {
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
			}
			client := httpClient
			if c.ConnectionMode == "relay" {
				relay.Headers(req, c.RelayDevice, c.RelayGroup, c.RelayPair)
				req.Header.Set("X-Replay-SHA256", art.SHA256)
				req.Header.Set("X-Replay-Size", strconv.FormatInt(art.Size, 10))
				client = relayMediaHTTP
			}
			r, e := client.Do(req)
			if e != nil {
				cancel()
				continue
			}
			transferID = r.Header.Get("X-Replay-Transfer")
			if r.StatusCode == 200 {
				offset = 0
			} else if r.StatusCode != 206 || !strings.HasPrefix(r.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", offset)) {
				r.Body.Close()
				cancel()
				return errors.New("节点 Range 响应无效")
			}
			flags := os.O_CREATE | os.O_WRONLY
			if offset == 0 {
				flags |= os.O_TRUNC
			} else {
				flags |= os.O_APPEND
			}
			f, e := os.OpenFile(part, flags, 0600)
			if e != nil {
				r.Body.Close()
				cancel()
				return e
			}
			_, copyErr := io.Copy(f, io.LimitReader(r.Body, art.Size-offset+1))
			f.Sync()
			f.Close()
			r.Body.Close()
			cancel()
			if copyErr != nil {
				continue
			}
		}
		hash, size, e := fileHash(part)
		if e != nil {
			continue
		}
		if size < art.Size {
			continue
		}
		if size != art.Size || hash != art.SHA256 {
			os.Remove(part)
			continue
		}
		if e = replaceFile(part, path); e != nil {
			return e
		}
		art.Path = path
		if c.ConnectionMode == "relay" && relay.IDPattern.MatchString(transferID) {
			req, e := relayRequest(a.ctx, c, "POST", "/v1/media-ack/"+transferID, nil)
			if e == nil {
				if res, e := relayControlHTTP.Do(req); e == nil {
					res.Body.Close()
				}
			}
		}
		return nil
	}
	return errors.New("下载失败或 SHA-256 不匹配；素材未加入可播库")
}
func (a *Service) acceptJob(j Job) (Job, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.role != "agent" {
		return j, errors.New("此端不是录制 Agent")
	}
	for _, existing := range a.s.Jobs {
		if existing.ID == j.ID {
			before, after := jsonCopy(existing), jsonCopy(j)
			before.Status = ""
			after.Status = ""
			before.Error = ""
			after.Error = ""
			before.Artifacts = nil
			after.Artifacts = nil
			x, _ := json.Marshal(before)
			y, _ := json.Marshal(after)
			if !bytes.Equal(x, y) {
				return j, errors.New("相同 job_id 的任务内容发生变化")
			}
			return existing, nil
		}
	}
	c := a.s.Config
	if a.localDemo.Phase == "armed" {
		return j, errors.New("录制资源已被占用")
	}
	if a.storageErr != "" {
		return j, errors.New(a.storageErr)
	}
	if !validID(j.ID) || j.Epoch != c.Epoch || j.Match != c.Match || j.Map != c.Map || c.Paused || c.CalibratedUntil < nowMS() {
		return j, errors.New("任务身份、会话、暂停状态或校准无效")
	}
	if j.Demo != (c.Mode == "demo") || j.Start < nowMS()+int64(c.Setup*1000) || j.End <= j.Start || j.End-j.Start > 30000 || len(j.Events) < 1 || len(j.Events) > 100 {
		return j, errors.New("任务窗口或模式无效")
	}
	if j.Player == "" || len(j.Player) > 100 {
		return j, errors.New("任务击杀归属身份无效")
	}
	if err := validateUtility(j.Utility); err != nil {
		return j, err
	}
	if !j.Demo && c.TrackingMode != "native" && j.Utility != nil && c.TrackingURL == "" {
		return j, errors.New("TRACKING_UNAVAILABLE：节点未配置道具镜头适配器")
	}
	if !j.Demo && j.Utility == nil && (j.Player == "" || j.Slot != c.Mappings[j.Player] || j.Slot < 1) {
		return j, errors.New("B 路已确认映射与任务不符")
	}
	seen := map[string]bool{}
	for i, e := range j.Events {
		targetOK := j.Utility == nil && e.Utility == nil || sameTarget(e, Event{Player: j.Player, Utility: j.Utility, Match: j.Match, Map: j.Map, Epoch: j.Epoch})
		if !validID(e.ID) || seen[e.ID] || !targetOK || !resolvedUtility(e.Utility) || e.Epoch != j.Epoch || e.Match != j.Match || e.Map != j.Map || e.GroupCount > 1 || e.Uncertainty > c.Guard || e.Uncertainty < 0 || e.Time-replayBeforeMS < j.Start || e.Time+replayAfterMS > j.End {
			return j, errors.New("事件不属于任务窗口或身份")
		}
		if e.Player == "" || i == 0 && e.Player != j.Player || e.Round != j.Events[0].Round || i > 0 && (e.Time < j.Events[i-1].Time || !compatible(j.Events[i-1], e, c)) {
			return j, errors.New("任务镜头顺序或切换间隔无效")
		}
		if !j.Demo && j.Utility == nil && c.Mappings[e.Player] <= 0 {
			return j, errors.New("B 路缺少切换目标的已确认映射")
		}
		if !j.Demo && j.Utility != nil && c.TrackingMode == "native" && !nativeTrackReady(e, c) {
			return j, errors.New("道具轨迹未覆盖事件窗口")
		}
		seen[e.ID] = true
	}
	if !j.Demo && j.Utility != nil && c.TrackingMode == "native" {
		u, err := mergedUtility(j.Events)
		if err != nil {
			return j, err
		}
		if len(u.Track) < 2 || u.Track[0].Time > j.Start || u.Track[len(u.Track)-1].Time < j.End {
			return j, errors.New("任务轨迹未覆盖完整录制窗口")
		}
		x, _ := json.Marshal(u)
		y, _ := json.Marshal(j.Utility)
		if !bytes.Equal(x, y) {
			return j, errors.New("任务轨迹与事件轨迹不一致")
		}
	}
	for _, existing := range a.s.Jobs {
		if reservesCamera(existing.Status) && j.Start-int64(c.Setup*1000) < existing.End+int64(c.Transition*1000) && j.End+int64(c.Transition*1000) > existing.Start-int64(c.Setup*1000) {
			return j, errors.New("任务与已排队录制窗口冲突")
		}
	}
	j.Status = "COMMITTED"
	j.Error = ""
	j.Artifacts = []Artifact{}
	a.s.Jobs = append(a.s.Jobs, j)
	a.active = true
	a.saveLocked()
	if a.storageErr != "" {
		a.active = false
		return j, errors.New(a.storageErr)
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.runJob(j, c)
		a.mu.Lock()
		a.active = a.pendingJobsLocked()
		a.mu.Unlock()
	}()
	return j, nil
}
