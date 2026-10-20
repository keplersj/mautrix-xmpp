package xmppx

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// ManagerEvents receives connection lifecycle callbacks from a Manager.
type ManagerEvents interface {
	// OnConnecting is called before each connection attempt.
	OnConnecting(attempt int)
	// OnSessionStarted is called with each new session. It should do the
	// session setup (roster, presence, MUC joins, catch-up). If it returns an
	// error, the session is closed and retried.
	OnSessionStarted(ctx context.Context, s *Session) error
	// OnDisconnected is called when a session ends or a connection attempt
	// fails and the manager will retry after retryIn.
	OnDisconnected(err error, retryIn time.Duration)
	// OnFatal is called when the manager gives up, for example because the
	// credentials were rejected. Run returns after this.
	OnFatal(err error)
}

// ManagerConfig controls reconnect behavior.
type ManagerConfig struct {
	InitialDelay time.Duration
	MaxDelay     time.Duration
	PingInterval time.Duration
	PingTimeout  time.Duration
}

// Manager keeps one XMPP account connected, reconnecting with exponential
// backoff and carrying unacknowledged messages across sessions.
type Manager struct {
	cfg     *Config
	mcfg    ManagerConfig
	log     zerolog.Logger
	handler Handler
	events  ManagerEvents

	mu      sync.Mutex
	current *Session
	cancel  context.CancelFunc
	stopped chan struct{}
	kick    chan struct{}
}

// NewManager creates a manager. Call Run to start it.
func NewManager(cfg *Config, mcfg ManagerConfig, log zerolog.Logger, handler Handler, events ManagerEvents) *Manager {
	if mcfg.InitialDelay <= 0 {
		mcfg.InitialDelay = 2 * time.Second
	}
	if mcfg.MaxDelay < mcfg.InitialDelay {
		mcfg.MaxDelay = 5 * time.Minute
	}
	if mcfg.PingTimeout <= 0 {
		mcfg.PingTimeout = 30 * time.Second
	}
	return &Manager{cfg: cfg, mcfg: mcfg, log: log, handler: handler, events: events, kick: make(chan struct{}, 1)}
}

// Session returns the current session, or nil while disconnected.
func (m *Manager) Session() *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// Start runs the manager in the background. Calling Start on a running
// manager does nothing.
func (m *Manager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.stopped = make(chan struct{})
	go func() {
		defer close(m.stopped)
		m.run(ctx)
	}()
}

// Stop disconnects and stops reconnecting. It waits for the manager to finish.
func (m *Manager) Stop() {
	m.mu.Lock()
	cancel, stopped, cur := m.cancel, m.stopped, m.current
	m.cancel = nil
	m.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	if cur != nil {
		cur.Close()
	}
	<-stopped
}

// Reconnect drops the current session (if any) and reconnects immediately,
// skipping the backoff delay.
func (m *Manager) Reconnect() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
	if s := m.Session(); s != nil {
		s.Close()
	}
}

// backoff returns the delay before attempt n (n >= 1), with ±20% jitter.
func backoff(initial, maxDelay time.Duration, n int) time.Duration {
	d := initial
	for i := 1; i < n && d < maxDelay; i++ {
		d *= 2
	}
	if d > maxDelay {
		d = maxDelay
	}
	jitter := time.Duration((rand.Float64()*0.4 - 0.2) * float64(d))
	return d + jitter
}

func (m *Manager) run(ctx context.Context) {
	var unacked [][]byte
	failures := 0
	for ctx.Err() == nil {
		m.events.OnConnecting(failures + 1)
		s, err := Connect(ctx, m.cfg, m.log, m.handler, unacked)
		if err == nil {
			unacked = nil
			m.mu.Lock()
			m.current = s
			m.mu.Unlock()
			setupCtx, cancel := context.WithCancel(ctx)
			go func() {
				// Abort setup if the session dies during it.
				select {
				case <-s.Done():
					cancel()
				case <-setupCtx.Done():
				}
			}()
			err = m.events.OnSessionStarted(setupCtx, s)
			cancel()
			if err != nil {
				m.log.Warn().Err(err).Msg("Session setup failed, reconnecting")
				s.Close()
			} else {
				failures = 0
				err = m.keepalive(ctx, s)
			}
			m.mu.Lock()
			m.current = nil
			m.mu.Unlock()
			<-s.Done()
			if err == nil {
				err = s.Err()
			}
			unacked = append(unacked, s.TakeUnacked()...)
		}
		if ctx.Err() != nil {
			return
		}
		var authErr *AuthError
		if errors.As(err, &authErr) && authErr.Permanent() {
			m.events.OnFatal(err)
			return
		}
		failures++
		delay := backoff(m.mcfg.InitialDelay, m.mcfg.MaxDelay, failures)
		if IsStreamError(err, "conflict") {
			// Another client took our resource; a new random one fixes that.
			m.cfg.Resource = conflictResource(m.cfg.Resource)
		}
		m.events.OnDisconnected(err, delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		case <-m.kick:
			failures = 0
		}
	}
}

func conflictResource(prev string) string {
	const suffixLen = 7 // "." + 6 hex chars
	base := prev
	if len(base) > suffixLen && base[len(base)-suffixLen] == '.' {
		base = base[:len(base)-suffixLen]
	}
	return base + "." + RandomID()[:6]
}

// keepalive pings the server until the session ends. A failed ping closes the
// session, which makes Serve return.
func (m *Manager) keepalive(ctx context.Context, s *Session) error {
	if m.mcfg.PingInterval <= 0 {
		select {
		case <-s.Done():
		case <-ctx.Done():
			s.Close()
		}
		return nil
	}
	ticker := time.NewTicker(m.mcfg.PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.Done():
			return nil
		case <-ctx.Done():
			s.Close()
			return nil
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, m.mcfg.PingTimeout)
			err := s.Ping(pingCtx)
			cancel()
			if err != nil && ctx.Err() == nil {
				m.log.Warn().Err(err).Msg("Keepalive ping failed, dropping connection")
				s.Close()
				return err
			}
			s.RequestAck(ctx)
		}
	}
}
