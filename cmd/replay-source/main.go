// replay-source turns real Demo/CSTV events into Replay events. A local growing
// recording may be tailed; historical files are exported unless an anchor is set.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"projectreplay/internal/replay"
	"strings"
	"time"
)

type tailReader struct {
	file *os.File
	ctx  context.Context
}

func (t tailReader) Read(b []byte) (int, error) {
	for {
		n, e := t.file.Read(b)
		if n > 0 || e != io.EOF {
			return n, e
		}
		position, err := t.file.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0, err
		}
		st, err := t.file.Stat()
		if err != nil {
			return 0, err
		}
		if st.Size() < position {
			return 0, fmt.Errorf("source was truncated; restart with a new source-id and epoch")
		}
		select {
		case <-t.ctx.Done():
			return 0, t.ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
func main() {
	demo := flag.String("demo", "", "Demo file")
	broadcast := flag.String("cstv", "", "CSTV+ HTTP broadcast URL (not a GOTV host:port)")
	tail := flag.Bool("tail", false, "Follow a recording that is still growing")
	source := flag.String("source-id", "", "Stable unique feed identity")
	match := flag.String("match", "", "Replay match name")
	mapName := flag.String("map", "", "Optional map name")
	epoch := flag.Int("epoch", 1, "Replay epoch")
	from := flag.Int64("from-tick", 0, "First server tick")
	to := flag.Int64("to-tick", 0, "Last server tick")
	api := flag.String("api", "", "Director base URL; absent exports JSONL to stdout")
	anchor := flag.Int64("wall-anchor", 0, "Unix milliseconds at source game time zero on B")
	delay := flag.Duration("arrival-delay", 0, "Explicit approximate B delay after each live event receipt; only tail/CSTV")
	uncertainty := flag.Float64("uncertainty", .15, "Wall-clock mapping uncertainty seconds")
	flag.Parse()
	if (*demo == "") == (*broadcast == "") || (*tail && *demo == "") || (*delay > 0 && (*anchor != 0 || (!*tail && *broadcast == ""))) || (*api != "" && *anchor == 0 && !(*delay > 0 && (*tail || *broadcast != ""))) {
		fmt.Fprintln(os.Stderr, "provide one source; API ingestion requires wall-anchor or live arrival-delay")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	var r io.Reader
	if *demo != "" {
		f, e := os.Open(*demo)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		defer f.Close()
		r = f
		if *tail {
			r = tailReader{f, ctx}
		}
	}
	if *api != "" {
		if !strings.HasPrefix(*api, "http://") && !strings.HasPrefix(*api, "https://") {
			fmt.Fprintln(os.Stderr, "api must use http(s)")
			os.Exit(2)
		}
	}
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	err := replay.ParseSource(ctx, r, *broadcast, replay.SourceOptions{Match: *match, Map: *mapName, SourceID: *source, Epoch: *epoch, FromTick: *from, ToTick: *to}, func(e replay.Event) error {
		offset := *anchor
		if *delay > 0 {
			offset = time.Now().Add(*delay).UnixMilli() - e.Time
		}
		if offset != 0 {
			e = replay.MapSourceEvent(e, offset, *uncertainty)
		}
		if *api == "" {
			return json.NewEncoder(os.Stdout).Encode(e)
		}
		data, err := json.Marshal(e)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(*api, "/")+"/api/events", bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			return fmt.Errorf("event %s: HTTP %d %s", e.ID, resp.StatusCode, body)
		}
		fmt.Fprintln(os.Stderr, "submitted", e.ID)
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
