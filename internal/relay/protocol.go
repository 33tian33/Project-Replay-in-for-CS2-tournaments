// Package relay provides the optional group-code WAN transport. LAN APIs do
// not depend on the relay protocol.
package relay

import (
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"regexp"
	"strings"
)

const MaxChunk = 1 << 20

var GroupPattern = regexp.MustCompile(`^[0-9]{4}$`)
var IDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var HashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func ID() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

type Hello struct {
	Role  string `json:"role,omitempty"`
	Group string `json:"group"`
	Name  string `json:"name"`
	Auto  bool   `json:"auto"`
}
type Node struct {
	PeerOnline bool   `json:"peer_online"`
	ID         string `json:"id"`
	Role       string `json:"role"`
	Group      string `json:"group"`
	Name       string `json:"name"`
	Auto       bool   `json:"auto"`
	Peer       string `json:"peer"`
	Pair       string `json:"pair"`
	Online     bool   `json:"online"`
	Pending    string `json:"pending"`
}
type Message struct {
	Type     string `json:"type"`
	ID       string `json:"id,omitempty"`
	Pair     string `json:"pair,omitempty"`
	Method   string `json:"method,omitempty"`
	Path     string `json:"path,omitempty"`
	Body     []byte `json:"body,omitempty"`
	Status   int    `json:"status,omitempty"`
	Deadline int64  `json:"deadline,omitempty"`
	Node     *Node  `json:"node,omitempty"`
	Artifact string `json:"artifact,omitempty"`
	Hash     string `json:"hash,omitempty"`
	Size     int64  `json:"size,omitempty"`
	Offset   int64  `json:"offset,omitempty"`
}

// Allowed is deliberately an API allowlist, never an arbitrary TCP/HTTP proxy.
func Allowed(method, path string) bool {
	u, e := url.ParseRequestURI(path)
	if e != nil || u.IsAbs() || u.Host != "" || strings.Contains(u.Path, "..") {
		return false
	}
	if u.RawQuery != "" && u.RawQuery != "agent=1" {
		return false
	}
	p := u.Path
	if method == "GET" {
		if p == "/api/time" || p == "/api/state" {
			return true
		}
		for _, prefix := range []string{"/api/jobs/", "/api/round-events/"} {
			if strings.HasPrefix(p, prefix) {
				return IDPattern.MatchString(strings.TrimPrefix(p, prefix))
			}
		}
	}
	if method == "POST" {
		switch p {
		case "/api/jobs", "/api/round-events", "/api/connect-gotv", "/api/teams", "/api/local-demo", "/api/timing/compact", "/api/cleanup":
			return true
		}
	}
	return false
}
