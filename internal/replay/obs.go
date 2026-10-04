package replay

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"net"
	"strings"
	"time"
)

type obs struct{ conn *websocket.Conn }

func digest(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.StdEncoding.EncodeToString(h[:])
}
func openOBS(c Config) (*obs, error) {
	d := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	conn, _, e := d.Dial(c.OBSURL, nil)
	if e != nil {
		return nil, e
	}
	o := &obs{conn}
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()
	conn.SetReadLimit(4 << 20)
	conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	var hello struct {
		Op int `json:"op"`
		D  struct {
			Authentication *struct {
				Salt      string `json:"salt"`
				Challenge string `json:"challenge"`
			} `json:"authentication"`
		} `json:"d"`
	}
	if e = conn.ReadJSON(&hello); e != nil {
		return nil, e
	}
	if hello.Op != 0 {
		return nil, fmt.Errorf("OBS 未发送 Hello")
	}
	identify := map[string]any{"rpcVersion": 1, "eventSubscriptions": 0}
	if hello.D.Authentication != nil {
		a := hello.D.Authentication
		identify["authentication"] = digest(digest(c.OBSPassword+a.Salt) + a.Challenge)
	}
	conn.SetWriteDeadline(time.Now().Add(4 * time.Second))
	if e = conn.WriteJSON(map[string]any{"op": 1, "d": identify}); e != nil {
		return nil, e
	}
	var result struct {
		Op int `json:"op"`
	}
	if e = conn.ReadJSON(&result); e != nil {
		return nil, e
	}
	if result.Op != 2 {
		return nil, fmt.Errorf("OBS 身份验证失败")
	}
	ok = true
	return o, nil
}
func (o *obs) close() { o.conn.Close() }
func (o *obs) call(kind string, data any) (map[string]any, error) {
	requestID := id()
	o.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	o.conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	if e := o.conn.WriteJSON(map[string]any{"op": 6, "d": map[string]any{"requestType": kind, "requestId": requestID, "requestData": data}}); e != nil {
		return nil, e
	}
	for {
		var r struct {
			Op int `json:"op"`
			D  struct {
				ID     string `json:"requestId"`
				Status struct {
					Result  bool   `json:"result"`
					Comment string `json:"comment"`
					Code    int    `json:"code"`
				} `json:"requestStatus"`
				Data map[string]any `json:"responseData"`
			} `json:"d"`
		}
		if e := o.conn.ReadJSON(&r); e != nil {
			return nil, e
		}
		if r.Op != 7 || r.D.ID != requestID {
			continue
		}
		if !r.D.Status.Result {
			return nil, fmt.Errorf("OBS %s (%d): %s", kind, r.D.Status.Code, r.D.Status.Comment)
		}
		return r.D.Data, nil
	}
}
func netCommand(address, command string) error {
	if strings.ContainsAny(command, "\r\n") {
		return fmt.Errorf("控制台命令含换行")
	}
	conn, e := net.DialTimeout("tcp", address, 2*time.Second)
	if e != nil {
		return e
	}
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, e = conn.Write([]byte(command + "\n"))
	return e
}
func number(m map[string]any, k string) float64     { n, _ := m[k].(float64); return n }
func stringField(m map[string]any, k string) string { s, _ := m[k].(string); return s }
func obj(m map[string]any, k string) map[string]any { o, _ := m[k].(map[string]any); return o }
func jsonCopy[T any](v T) T {
	b, _ := json.Marshal(v)
	var copy T
	json.Unmarshal(b, &copy)
	return copy
}
