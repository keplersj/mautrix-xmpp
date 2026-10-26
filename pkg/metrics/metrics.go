// Package metrics holds the bridge's Prometheus metrics.
//
// Every label value comes from a fixed set chosen by the bridge. Nothing
// derived from JIDs, room addresses, server names or error text is used as a
// label, to keep cardinality bounded and to keep personal data out of
// metrics. All methods are safe to call on a nil *Metrics.
package metrics

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"mellium.im/xmpp/stanza"

	"github.com/keplersj/mautrix-xmpp/pkg/xmppx"
)

const namespace = "mautrix_xmpp"

// Login states reported by the logins gauge.
const (
	StateConnecting     = "connecting"
	StateConnected      = "connected"
	StateDisconnected   = "disconnected"
	StateBadCredentials = "bad_credentials"
	StateStopped        = "stopped"
)

// LoginStates lists every login state, so each one is always reported.
var LoginStates = []string{StateConnecting, StateConnected, StateDisconnected, StateBadCredentials, StateStopped}

// Snapshot is the per-login state read at scrape time.
type Snapshot struct {
	// Logins counts logins by state.
	Logins map[string]int
	// Unacked is the number of outbound stanzas waiting for a stream
	// management ack, summed over all sessions.
	Unacked int
}

// Metrics is the bridge's metric registry.
type Metrics struct {
	reg *prometheus.Registry

	snapshot func() Snapshot

	connectAttempts prometheus.Counter
	disconnects     *prometheus.CounterVec
	stanzas         *prometheus.CounterVec
	iqDuration      *prometheus.HistogramVec
	mamMessages     prometheus.Counter
	resent          prometheus.Counter
	sendDuration    *prometheus.HistogramVec
	mediaTransfers  *prometheus.CounterVec
	mediaBytes      *prometheus.CounterVec
}

var _ xmppx.Observer = (*Metrics)(nil)

// New creates the metrics and registers them, together with the Go runtime
// and process collectors, on a private registry.
func New(version, commit string) *Metrics {
	m := &Metrics{reg: prometheus.NewRegistry()}
	m.connectAttempts = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Name: "connect_attempts_total",
		Help: "XMPP connection attempts.",
	})
	m.disconnects = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "disconnects_total",
		Help: "XMPP sessions that ended or connection attempts that failed, by reason.",
	}, []string{"reason"})
	m.stanzas = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "stanzas_total",
		Help: "XMPP stanzas on the wire.",
	}, []string{"direction", "kind"})
	m.iqDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Name: "iq_duration_seconds",
		Help:    "Round trip time of IQs sent by the bridge.",
		Buckets: []float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60},
	}, []string{"op", "result"})
	m.mamMessages = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Name: "mam_messages_total",
		Help: "Messages fetched from message archives (XEP-0313).",
	})
	m.resent = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Name: "resent_messages_total",
		Help: "Messages re-sent on a new session because the server never acknowledged them.",
	})
	m.sendDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Name: "matrix_send_duration_seconds",
		Help: "Time to send a Matrix message to XMPP, including waiting for the server's ack.",
		// The ack wait is capped at 10s, so the top buckets catch timeouts.
		Buckets: []float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 15},
	}, []string{"result"})
	m.mediaTransfers = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "media_transfers_total",
		Help: "Files downloaded from XMPP links or uploaded with HTTP file upload (XEP-0363).",
	}, []string{"direction", "result"})
	m.mediaBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "media_bytes_total",
		Help: "Bytes of successfully transferred files.",
	}, []string{"direction"})
	buildInfo := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Name: "build_info",
		Help:        "Bridge version.",
		ConstLabels: prometheus.Labels{"version": version, "commit": commit},
	})
	buildInfo.Set(1)

	m.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		buildInfo, m.connectAttempts, m.disconnects, m.stanzas, m.iqDuration,
		m.mamMessages, m.resent, m.sendDuration, m.mediaTransfers, m.mediaBytes,
		snapshotCollector{m},
	)
	// Pre-create the series alerts are likely to use, so they exist at 0.
	for _, r := range DisconnectReasons {
		m.disconnects.WithLabelValues(r)
	}
	for _, dir := range []string{"in", "out"} {
		for _, kind := range []string{"message", "presence", "iq"} {
			m.stanzas.WithLabelValues(dir, kind)
		}
	}
	return m
}

// Handler serves the metrics in the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{Registry: m.reg})
}

// SetSnapshotFunc sets the function that reports per-login state at scrape
// time.
func (m *Metrics) SetSnapshotFunc(fn func() Snapshot) {
	if m != nil {
		m.snapshot = fn
	}
}

// ConnectAttempt counts one connection attempt.
func (m *Metrics) ConnectAttempt() {
	if m != nil {
		m.connectAttempts.Inc()
	}
}

// Disconnect counts a dropped session or failed attempt. reason must be one
// of DisconnectReasons.
func (m *Metrics) Disconnect(reason string) {
	if m != nil {
		m.disconnects.WithLabelValues(reason).Inc()
	}
}

// Send results for MatrixSend.
const (
	SendOK           = "ok"
	SendAckTimeout   = "ack_timeout"
	SendUnconfirmed  = "unconfirmed" // written, but the server has no stream management
	SendNotConnected = "not_connected"
	SendFailed       = "failed"
)

// MatrixSend records sending one Matrix message to XMPP.
func (m *Metrics) MatrixSend(result string, d time.Duration) {
	if m != nil {
		m.sendDuration.WithLabelValues(result).Observe(d.Seconds())
	}
}

// Media transfer directions and results.
const (
	MediaDownload = "download"
	MediaUpload   = "upload"

	MediaOK       = "ok"
	MediaTooLarge = "too_large"
	MediaFailed   = "failed"
)

// MediaTransfer records one file download or upload.
func (m *Metrics) MediaTransfer(direction, result string, bytes int) {
	if m == nil {
		return
	}
	m.mediaTransfers.WithLabelValues(direction, result).Inc()
	if result == MediaOK {
		m.mediaBytes.WithLabelValues(direction).Add(float64(bytes))
	}
}

// Stanza implements xmppx.Observer.
func (m *Metrics) Stanza(outbound bool, name string) {
	if m == nil {
		return
	}
	dir := "in"
	if outbound {
		dir = "out"
	}
	m.stanzas.WithLabelValues(dir, name).Inc()
}

// IQDone implements xmppx.Observer.
func (m *Metrics) IQDone(op string, d time.Duration, err error) {
	if m != nil {
		m.iqDuration.WithLabelValues(op, iqResult(err)).Observe(d.Seconds())
	}
}

func iqResult(err error) string {
	var se stanza.Error
	switch {
	case err == nil:
		return "ok"
	case errors.As(err, &se):
		return "error"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "failed"
	}
}

// MAMPage implements xmppx.Observer.
func (m *Metrics) MAMPage(messages int) {
	if m != nil {
		m.mamMessages.Add(float64(messages))
	}
}

// Resent implements xmppx.Observer.
func (m *Metrics) Resent(messages int) {
	if m != nil {
		m.resent.Add(float64(messages))
	}
}

var (
	loginsDesc = prometheus.NewDesc(namespace+"_logins",
		"Logged in XMPP accounts by connection state.", []string{"state"}, nil)
	unackedDesc = prometheus.NewDesc(namespace+"_unacked_stanzas",
		"Outbound stanzas waiting for a stream management (XEP-0198) ack.", nil, nil)
)

// snapshotCollector reads per-login state when scraped, so the gauges can't
// drift from the real state.
type snapshotCollector struct{ m *Metrics }

func (c snapshotCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- loginsDesc
	ch <- unackedDesc
}

func (c snapshotCollector) Collect(ch chan<- prometheus.Metric) {
	var snap Snapshot
	if c.m.snapshot != nil {
		snap = c.m.snapshot()
	}
	for _, state := range LoginStates {
		ch <- prometheus.MustNewConstMetric(loginsDesc, prometheus.GaugeValue, float64(snap.Logins[state]), state)
	}
	ch <- prometheus.MustNewConstMetric(unackedDesc, prometheus.GaugeValue, float64(snap.Unacked))
}
