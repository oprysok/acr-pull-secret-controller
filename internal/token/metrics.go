package token

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	refreshTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "acr_pull_secret_token_refresh_total",
		Help: "Token renewals (Entra token + ACR exchange), by result.",
	}, []string{"result"})

	expiryTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "acr_pull_secret_token_expiry_timestamp_seconds",
		Help: "Expiry of the current ACR refresh token, per registry.",
	}, []string{"registry"})
)

func init() {
	metrics.Registry.MustRegister(refreshTotal, expiryTimestamp)
}
