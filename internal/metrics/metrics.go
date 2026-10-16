// Package metrics defines the Prometheus metrics of design doc section
// 12.3, served at /metrics.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry holds every mockfix metric plus the Go runtime collectors.
var Registry = prometheus.NewRegistry()

var (
	SessionsLoggedOn = gauge("mockfix_sessions_logged_on", "Client and monitor sessions currently logged on.")
	FIXMessages      = counter("mockfix_fix_messages_total", "FIX messages by session, direction (in/out) and MsgType.",
		"session", "direction", "msg_type")
	Ticks             = counter("mockfix_ticks_total", "Price ticks generated.", "symbol")
	MDSubscriptions   = gaugeVec("mockfix_md_subscriptions", "Active market data subscriptions.", "symbol")
	MDConflated       = counter("mockfix_md_conflated_total", "Ticks dropped because a newer tick replaced them.", "session")
	MDBatchSize       = histogram("mockfix_md_batch_size", "Symbols per batched X message.", []float64{1, 2, 5, 10, 20, 50, 100}, "session")
	Orders            = counter("mockfix_orders_total", "Orders by session, matched rule and result.", "session", "rule", "result")
	OrderLatency      = histogram("mockfix_order_latency_seconds", "Handling time of order messages.", prometheus.ExponentialBuckets(0.00005, 2, 14), "msg_type")
	MirrorDropped     = counter0("mockfix_mirror_dropped_total", "Mirror log messages dropped because the buffer was full.")
	ConfigVersion     = gauge("mockfix_config_version", "Running config version.")
	ConfigReloadError = counter0("mockfix_config_reload_errors_total", "Config changes rejected by validation.")
)

func init() {
	Registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
}

// Handler serves the registry in the Prometheus text format.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{})
}

func counter(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
	Registry.MustRegister(c)
	return c
}

func counter0(name, help string) prometheus.Counter {
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: name, Help: help})
	Registry.MustRegister(c)
	return c
}

func gauge(name, help string) prometheus.Gauge {
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: help})
	Registry.MustRegister(g)
	return g
}

func gaugeVec(name, help string, labels ...string) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
	Registry.MustRegister(g)
	return g
}

func histogram(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: buckets}, labels)
	Registry.MustRegister(h)
	return h
}
