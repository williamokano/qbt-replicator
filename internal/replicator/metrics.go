package replicator

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics holds the Prometheus collectors exposed on /metrics.
type Metrics struct {
	Registry *prometheus.Registry

	up        *prometheus.GaugeVec
	upSpeed   *prometheus.GaugeVec
	dlSpeed   *prometheus.GaugeVec
	torrents  *prometheus.GaugeVec
	replicas  *prometheus.GaugeVec
	actions   *prometheus.CounterVec
	runSecs   prometheus.Histogram
	lastRunTS prometheus.Gauge
}

// NewMetrics creates and registers all collectors on a private registry.
func NewMetrics() *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		up: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "qbtr_instance_up", Help: "1 if the qBittorrent instance is reachable.",
		}, []string{"instance"}),
		upSpeed: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "qbtr_instance_upload_bytes_per_second", Help: "Current upload speed of the instance.",
		}, []string{"instance"}),
		dlSpeed: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "qbtr_instance_download_bytes_per_second", Help: "Current download speed of the instance.",
		}, []string{"instance"}),
		torrents: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "qbtr_instance_torrents", Help: "Number of torrents on the instance.",
		}, []string{"instance"}),
		replicas: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "qbtr_instance_replicas", Help: "Number of replica torrents on the instance.",
		}, []string{"instance"}),
		actions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "qbtr_actions_total", Help: "Executed actions by type and result.",
		}, []string{"type", "result"}),
		runSecs: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "qbtr_reconcile_duration_seconds", Help: "Duration of reconcile passes.",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 12),
		}),
		lastRunTS: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "qbtr_last_reconcile_timestamp_seconds", Help: "Unix time of the last reconcile pass.",
		}),
	}
	m.Registry.MustRegister(m.up, m.upSpeed, m.dlSpeed, m.torrents, m.replicas, m.actions, m.runSecs, m.lastRunTS,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

func (m *Metrics) observeSnapshot(s *Snapshot, replicaTag string) {
	for name, st := range s.Instances {
		if !st.Healthy {
			m.up.WithLabelValues(name).Set(0)
			continue
		}
		m.up.WithLabelValues(name).Set(1)
		m.upSpeed.WithLabelValues(name).Set(float64(st.Transfer.UpSpeed))
		m.dlSpeed.WithLabelValues(name).Set(float64(st.Transfer.DlSpeed))
		m.torrents.WithLabelValues(name).Set(float64(len(st.Torrents)))
		n := 0
		for _, t := range st.Torrents {
			if t.HasTag(replicaTag) {
				n++
			}
		}
		m.replicas.WithLabelValues(name).Set(float64(n))
	}
}

func (m *Metrics) observeRun(r *RunReport) {
	m.runSecs.Observe(r.Duration.Seconds())
	m.lastRunTS.Set(float64(r.Started.Unix()))
	for _, res := range r.Results {
		result := "ok"
		switch {
		case res.DryRun:
			result = "dry_run"
		case res.Error != "":
			result = "error"
		}
		m.actions.WithLabelValues(string(res.Action.Type), result).Inc()
	}
}
