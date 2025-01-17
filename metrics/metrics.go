package metrics

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type ScanType string

const (
	ScanTypeImage           ScanType = "image"
	ScanTypeKubeBench       ScanType = "kube-bench"
	ScanTypeKubeBenchCached ScanType = "kube-bench-cached"
	ScanTypeLinter          ScanType = "linter"
	ScanTypeCloud           ScanType = "cloud"
)

type ScanStatus string

const (
	ScanStatusOK    ScanStatus = "ok"
	ScanStatusError ScanStatus = "error"
)

type timeSinceFunc func(t time.Time) time.Duration

// Used to override time sensitive properties in tests.
var timeSinceFn = timeSinceFunc(func(t time.Time) time.Duration {
	return time.Since(t)
})

var (
	scansTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "castai_security_agent_scans_total",
		Help: "Counter tracking scans and statuses",
	}, []string{"scan_type", "scan_status"})

	scansDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "castai_security_agent_scans_duration",
		Help:    "Histogram tracking scan durations in seconds",
		Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 15, 20, 30},
	}, []string{"scan_type"})

	deltasSentTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "castai_security_agent_deltas_total",
		Help: "Counter tracking deltas sent",
	})

	imagesTotalCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "castai_security_agent_images",
		Help: "Gauge for tracking container images count",
	})

	imagesPendingCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "castai_security_agent_pending_images",
		Help: "Gauge for tracking pending container images count",
	})
)

func init() {
	prometheus.MustRegister(
		scansTotal,
		scansDuration,
		deltasSentTotal,
		imagesTotalCount,
		imagesPendingCount,
	)
}

func scanStatus(err error) ScanStatus {
	if err != nil && !errors.Is(err, context.Canceled) {
		return ScanStatusError
	}
	return ScanStatusOK
}

func IncScansTotal(scanType ScanType, err error) {
	scansTotal.WithLabelValues(string(scanType), string(scanStatus(err))).Inc()
}

func SetTotalImagesCount(v int) {
	imagesTotalCount.Set(float64(v))
}

func SetPendingImagesCount(v int) {
	imagesPendingCount.Set(float64(v))
}

func ObserveScanDuration(scanType ScanType, start time.Time) {
	dur := timeSinceFn(start)
	scansDuration.WithLabelValues(string(scanType)).Observe(dur.Seconds())
}

func IncDeltasSentTotal() {
	deltasSentTotal.Inc()
}
