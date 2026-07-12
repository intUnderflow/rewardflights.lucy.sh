// Package alertapi is the public HTTP surface the website calls to manage
// Web Push subscriptions. The watcher serves it (behind a cloudflared
// tunnel), so it is written defensively: bounded bodies, short timeouts, a
// per-client token bucket, and a strict CORS origin allowlist.
//
// There is deliberately no authentication: in the Web Push model, knowing an
// endpoint IS the capability to manage it. The endpoint is minted by the
// browser's push service for one browser profile, is unguessable, and is the
// only identifier we store — so possession is the credential.
package alertapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/alertstore"
)

// Config parameterizes the API server.
type Config struct {
	Addr       string            // listen address, e.g. 127.0.0.1:8787
	Store      *alertstore.Store // subscription store (required)
	RatePerMin int               // requests per minute per client (default 60)
	Burst      int               // token-bucket burst (default 20)
	Logf       func(string, ...any)
	Now        func() time.Time // injected in tests; defaults to time.Now
}

// maxBody bounds request bodies: a subscription with 60 topics is ~3KB.
const maxBody = 16 << 10

// allowedOrigins is the exact production origin; dev origins are matched
// separately (any port on loopback).
const prodOrigin = "https://rewardflights.lucy.sh"

// Server is the running API.
type Server struct {
	cfg     Config
	http    *http.Server
	limiter *limiter
}

// New builds the API server (does not listen yet).
func New(cfg Config) *Server {
	if cfg.RatePerMin <= 0 {
		cfg.RatePerMin = 60
	}
	if cfg.Burst <= 0 {
		cfg.Burst = 20
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &Server{
		cfg: cfg,
		limiter: newLimiter(
			float64(cfg.RatePerMin)/60, // tokens per second
			float64(cfg.Burst), cfg.Now,
		),
	}
	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.Handler(),
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	return s
}

// Handler returns the fully wrapped mux (exported for tests).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /subscribe", s.handleSubscribe)
	mux.HandleFunc("POST /unsubscribe", s.handleUnsubscribe)
	mux.HandleFunc("GET /topics", s.handleTopics)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	return s.withCORS(s.withRateLimit(mux))
}

// ListenAndServe runs the server until the context is cancelled, then shuts
// down gracefully.
func (s *Server) ListenAndServe(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() {
		err := s.http.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.http.Shutdown(shutdownCtx)
	}
}

// subscribeRequest is the POST /subscribe body.
type subscribeRequest struct {
	Endpoint string   `json:"endpoint"`
	P256dh   string   `json:"p256dh"`
	Auth     string   `json:"auth"`
	Topics   []string `json:"topics"`
}

func (s *Server) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	var req subscribeRequest
	if !decode(w, r, &req) {
		return
	}
	topics, err := s.cfg.Store.Upsert(alertstore.Subscription{
		Endpoint: req.Endpoint, P256dh: req.P256dh, Auth: req.Auth, Topics: req.Topics,
	})
	switch {
	case errors.Is(err, alertstore.ErrFull):
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"ok": false, "error": "subscription store is full",
		})
		return
	case err != nil:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if topics == nil {
		topics = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "topics": topics})
}

func (s *Server) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Endpoint == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "endpoint is required"})
		return
	}
	s.cfg.Store.Remove(req.Endpoint)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleTopics(w http.ResponseWriter, r *http.Request) {
	endpoint := r.URL.Query().Get("endpoint")
	if endpoint == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "endpoint is required"})
		return
	}
	topics := s.cfg.Store.Topics(endpoint)
	if topics == nil {
		topics = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"topics": topics})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"subs":   s.cfg.Store.Count(),
		"topics": len(s.cfg.Store.ActiveTopics()),
	})
}

// decode reads a bounded JSON body. It writes the error response itself and
// reports whether decoding succeeded.
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"ok": false, "error": "request body too large",
			})
			return false
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// withCORS applies the origin allowlist and answers preflights.
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); allowedOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "content-type")
			w.Header().Set("Access-Control-Max-Age", "86400")
		}
		if r.Method == http.MethodOptions {
			// Preflight: headers above are the whole answer. An origin that
			// is not allowed simply gets no CORS headers and the browser
			// blocks the real request.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allowedOrigin permits the production site plus loopback dev origins on any
// port. Note CORS does not protect the API (a non-browser client ignores it);
// it exists so a hostile PAGE cannot drive a visitor's browser into managing
// their subscription. The endpoint-as-capability model is the real control.
func allowedOrigin(origin string) bool {
	if origin == prodOrigin {
		return true
	}
	return strings.HasPrefix(origin, "http://127.0.0.1:") ||
		strings.HasPrefix(origin, "http://localhost:") ||
		origin == "http://127.0.0.1" || origin == "http://localhost"
}

// withRateLimit applies the per-client token bucket.
func (s *Server) withRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, retryAfter := s.limiter.allow(clientIP(r)); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"ok": false, "error": "rate limit exceeded",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP prefers Cloudflare's client header (the API is served behind a
// cloudflared tunnel, so RemoteAddr is always the tunnel itself).
func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// limiter is a per-client token bucket with lazy refill and periodic eviction
// of idle buckets (so a flood of distinct IPs cannot grow memory forever).
type limiter struct {
	rate  float64 // tokens per second
	burst float64
	now   func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	seen   time.Time
}

func newLimiter(rate, burst float64, now func() time.Time) *limiter {
	return &limiter{rate: rate, burst: burst, now: now, buckets: map[string]*bucket{}}
}

// allow consumes a token for client, reporting whether the request may
// proceed and, when not, a Retry-After in whole seconds.
func (l *limiter) allow(client string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()

	b, ok := l.buckets[client]
	if !ok {
		if len(l.buckets) > 10000 {
			l.evict(now)
		}
		b = &bucket{tokens: l.burst, seen: now}
		l.buckets[client] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.seen).Seconds()*l.rate)
	b.seen = now

	if b.tokens < 1 {
		retry := int((1 - b.tokens) / l.rate)
		return false, max(retry, 1)
	}
	b.tokens--
	return true, 0
}

// evict drops buckets idle long enough to have fully refilled. Caller holds
// the lock.
func (l *limiter) evict(now time.Time) {
	idle := time.Duration(l.burst/l.rate) * time.Second
	for client, b := range l.buckets {
		if now.Sub(b.seen) > idle {
			delete(l.buckets, client)
		}
	}
}
