package metrics

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/pprof"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// Server serves /metrics, /healthz, /readyz and optionally /debug/pprof/.
type Server struct {
	srv   *http.Server
	ln    net.Listener
	log   zerolog.Logger
	ready atomic.Bool
}

// Listen binds addr and returns a server that is not serving yet; call Serve.
func Listen(addr string, m *Metrics, enablePprof bool, log zerolog.Logger) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln, log: log}
	s.srv = &http.Server{Handler: s.mux(m, enablePprof), ReadHeaderTimeout: 10 * time.Second}
	return s, nil
}

// mux builds the routes on a private ServeMux. mautrix imports net/http/pprof,
// which registers itself on http.DefaultServeMux, so that must never be used
// here or pprof would be exposed regardless of the setting.
func (s *Server) mux(m *Metrics, enablePprof bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	// Liveness only says the process is serving. It deliberately ignores XMPP
	// connection state, so one account with a wrong password can't get the
	// bridge restarted.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !s.ready.Load() {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	if enablePprof {
		mux.HandleFunc("GET /debug/pprof/", pprof.Index)
		mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	}
	return mux
}

// Addr returns the bound address.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Serve serves requests in the background.
func (s *Server) Serve() {
	s.log.Info().Stringer("address", s.ln.Addr()).Msg("Serving metrics")
	go func() {
		if err := s.srv.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error().Err(err).Msg("Metrics server failed")
		}
	}()
}

// SetReady sets what /readyz reports.
func (s *Server) SetReady(ready bool) { s.ready.Store(ready) }

// Close shuts the server down, waiting briefly for requests in flight.
func (s *Server) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx)
}
