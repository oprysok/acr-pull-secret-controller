package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var syncTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "acr_pull_secret_sync_total",
	Help: "Pull secret reconciliations, by result (updated, unchanged, removed, token_error, error).",
}, []string{"result"})

func init() {
	metrics.Registry.MustRegister(syncTotal)
}
