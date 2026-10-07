package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"ca.punkscience.tendrils/internal/config"
	"ca.punkscience.tendrils/internal/engine"
	"ca.punkscience.tendrils/internal/index"
	"ca.punkscience.tendrils/internal/keys"
)

// statusSnapshot is what a running daemon reports over its loopback status
// endpoint, and what the CLI reconstructs locally when no daemon is running.
type statusSnapshot struct {
	Identity      string    `json:"identity"`
	SyncRoot      string    `json:"sync_root"`
	Relays        []string  `json:"relays"`
	Storage       []string  `json:"storage"`
	LastReconcile time.Time `json:"last_reconcile"`
	Pending       int       `json:"pending"`
	Conflicts     int       `json:"conflicts"`
	// Deferred counts paths that keep failing and are waiting out a retry
	// backoff. They are included in Pending — this says how much of it is stuck.
	Deferred int `json:"deferred"`
	// Unavailable counts paths the last scan could not observe; they are neither
	// synced nor deleted until it can.
	Unavailable int `json:"unavailable"`
	// Blocked counts paths this device cannot sync — invalid names, names the
	// platform cannot represent, case collisions. Left untouched here.
	Blocked int `json:"blocked"`
	// Paused says why the daemon is doing no work at all, e.g. the sync root is
	// missing or is not the enrolled folder.
	Paused  string `json:"paused,omitempty"`
	Syncing bool   `json:"syncing"`
	// Per-file progress within the current pass (meaningful only while Syncing).
	Done      int    `json:"done"`
	Total     int    `json:"total"`
	Current   string `json:"current"`   // path being acted on now
	Operation string `json:"operation"` // verb for the current action
	LastError string `json:"last_error"`
}

// syncState tracks the live reconcile state the index cannot show: whether a
// pass is running, its per-file position, the outstanding-work counts from the
// last pass, and the error from the last pass (empty on success). It is the
// daemon's cache so a status query never pays for its own full-tree rescan.
type syncState struct {
	mu       sync.Mutex
	syncing  bool
	lastErr  string
	progress engine.Progress
	stats    engine.Stats
}

func (s *syncState) begin() {
	s.mu.Lock()
	s.syncing = true
	s.progress = engine.Progress{}
	s.mu.Unlock()
}

func (s *syncState) end(err error) {
	s.mu.Lock()
	s.syncing = false
	s.progress = engine.Progress{}
	if err != nil {
		s.lastErr = err.Error()
	} else {
		s.lastErr = ""
	}
	s.mu.Unlock()
}

// setProgress is the engine's OnProgress callback; called from the sync goroutine.
func (s *syncState) setProgress(p engine.Progress) {
	s.mu.Lock()
	s.progress = p
	s.mu.Unlock()
}

// setStats is the engine's OnStats callback; the counts persist across passes
// (begin/end never clear them) so an idle daemon still reports the last known
// outstanding work.
func (s *syncState) setStats(st engine.Stats) {
	s.mu.Lock()
	s.stats = st
	s.mu.Unlock()
}

func (s *syncState) read() (syncing bool, lastErr string, p engine.Progress, st engine.Stats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.syncing, s.lastErr, s.progress, s.stats
}

// statusServer answers the CLI's status query for a running daemon. Reads on the
// shared index handle are safe alongside the engine's writes (bbolt allows
// concurrent read transactions with one writer inside a process).
type statusServer struct {
	id    *keys.Identity
	cfg   *config.Config
	idx   *index.Store
	state *syncState
}

func (srv *statusServer) handler(w http.ResponseWriter, _ *http.Request) {
	// Cheap only: a single index read for the last-reconcile time, plus the
	// counts and progress the engine already cached during its passes. No scan
	// here — rehashing a large tree on every query is what made this time out.
	last, err := srv.idx.LastReconcile()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	syncing, lastErr, prog, stats := srv.state.read()
	npub, _ := srv.id.Npub()
	snap := statusSnapshot{
		Identity:      npub,
		SyncRoot:      srv.cfg.SyncRoot,
		Relays:        srv.cfg.Relays,
		Storage:       srv.cfg.BlossomServers,
		LastReconcile: last,
		Pending:       stats.Pending,
		Conflicts:     stats.Conflicts,
		Deferred:      stats.Deferred,
		Unavailable:   stats.Unavailable,
		Blocked:       stats.Blocked,
		Paused:        stats.Paused,
		Syncing:       syncing,
		Done:          prog.Done,
		Total:         prog.Total,
		Current:       prog.Path,
		Operation:     prog.Op,
		LastError:     lastErr,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snap)
}

// serveStatus starts the loopback status endpoint, records its address so the
// CLI can find it, and returns a stop func that shuts it down and removes the
// address file. Binding loopback exposes read-only sync state to any local user;
// on a single-user desktop that is acceptable (identity is public anyway). A
// failure to start is logged, not fatal: the daemon keeps syncing.
func serveStatus(srv *statusServer, log *slog.Logger) (stop func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Warn("status endpoint unavailable: could not bind loopback", "err", err)
		return func() {}
	}
	addrPath, err := config.DaemonAddrPath()
	if err != nil {
		log.Warn("status endpoint unavailable: could not locate state dir", "err", err)
		ln.Close()
		return func() {}
	}
	if err := os.WriteFile(addrPath, []byte(ln.Addr().String()), 0o600); err != nil {
		log.Warn("status endpoint unavailable: could not record address", "err", err)
		ln.Close()
		return func() {}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/status", srv.handler)
	httpSrv := &http.Server{Handler: mux}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Warn("status endpoint stopped", "err", err)
		}
	}()

	return func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		_ = os.Remove(addrPath)
	}
}

// queryDaemon asks a running daemon for its status. running is false (with a nil
// error) when no daemon is listening — the CLI then reads the index directly,
// which is safe precisely because no daemon holds the lock. A non-nil error
// means the daemon is up but the query failed: the caller must NOT fall back to
// the index (the daemon holds the lock; the read would only time out).
func queryDaemon() (snap statusSnapshot, running bool, err error) {
	addrPath, err := config.DaemonAddrPath()
	if err != nil {
		return snap, false, nil
	}
	raw, err := os.ReadFile(addrPath)
	if err != nil {
		return snap, false, nil // no address file → no daemon
	}
	addr := strings.TrimSpace(string(raw))

	// A dial probe separates "daemon down / stale address file" from "daemon up
	// but slow" (a large tree scan can take a while). Only the former may safely
	// fall back to opening the index directly.
	conn, derr := net.DialTimeout("tcp", addr, time.Second)
	if derr != nil {
		return snap, false, nil // not running, or the address file is stale
	}
	conn.Close()

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("http://" + addr + "/status")
	if err != nil {
		return snap, true, fmt.Errorf("daemon is running but its status endpoint did not answer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return snap, true, fmt.Errorf("daemon status endpoint returned %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return snap, true, fmt.Errorf("decode daemon status: %w", err)
	}
	return snap, true, nil
}
