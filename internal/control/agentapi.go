package control

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MaxWait bounds a single wait request.
const MaxWait = 10 * time.Minute

// AgentHandler serves the agent's view of the control state, meant for a
// Unix socket that only the desktop account can reach:
//
//	GET  /state                     current snapshot
//	POST /request {"reason": "..."} ask the human to take over
//	GET  /wait?seq=N&timeout=S      block until seq changes (at most S seconds)
//
// The agent cannot give itself control: only the human does that.
func AgentHandler(s *State) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.Get())
	})
	mux.HandleFunc("POST /request", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		reason := strings.TrimSpace(req.Reason)
		if reason == "" {
			http.Error(w, "reason is required", http.StatusBadRequest)
			return
		}
		writeJSON(w, s.RequestHuman(reason))
	})
	mux.HandleFunc("GET /wait", func(w http.ResponseWriter, r *http.Request) {
		seq, err := strconv.ParseUint(r.URL.Query().Get("seq"), 10, 64)
		if err != nil {
			http.Error(w, "seq is required", http.StatusBadRequest)
			return
		}
		timeout := MaxWait
		if v := r.URL.Query().Get("timeout"); v != "" {
			secs, err := strconv.Atoi(v)
			if err != nil || secs < 0 {
				http.Error(w, "invalid timeout", http.StatusBadRequest)
				return
			}
			timeout = min(time.Duration(secs)*time.Second, MaxWait)
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		snap, _ := s.Wait(ctx, seq) // on timeout, the unchanged snapshot is the answer
		writeJSON(w, snap)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

// Client talks to AgentHandler over a Unix socket.
type Client struct {
	http *http.Client
}

// NewClient returns a client for the socket at path.
func NewClient(path string) *Client {
	return &Client{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}}
}

// State returns the current snapshot.
func (c *Client) State(ctx context.Context) (Snapshot, error) {
	return c.do(ctx, http.MethodGet, "/state", nil)
}

// RequestHuman asks the human to take over.
func (c *Client) RequestHuman(ctx context.Context, reason string) (Snapshot, error) {
	body, _ := json.Marshal(map[string]string{"reason": reason})
	return c.do(ctx, http.MethodPost, "/request", strings.NewReader(string(body)))
}

// Wait blocks until the sequence number differs from seq or timeout elapses,
// and returns the snapshot at that point.
func (c *Client) Wait(ctx context.Context, seq uint64, timeout time.Duration) (Snapshot, error) {
	q := url.Values{"seq": {strconv.FormatUint(seq, 10)}, "timeout": {strconv.Itoa(int(timeout.Seconds()))}}
	return c.do(ctx, http.MethodGet, "/wait?"+q.Encode(), nil)
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader) (Snapshot, error) {
	var snap Snapshot
	req, err := http.NewRequestWithContext(ctx, method, "http://control"+path, body)
	if err != nil {
		return snap, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return snap, fmt.Errorf("control service unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return snap, fmt.Errorf("control service: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return snap, json.NewDecoder(resp.Body).Decode(&snap)
}
