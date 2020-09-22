package allocator

import (
	purelbv1 "purelb.io/pkg/apis/v1"

	"github.com/prometheus/client_golang/prometheus"
)

const subsystem = "address_pool"

var (
	labelNames = []string{"pool"}

	poolCapacity = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: purelbv1.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "size",
		Help:      "Number of addresses in the pool",
	}, labelNames)

	poolActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: purelbv1.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "addresses_in_use",
		Help:      "Number of addresses allocated from the pool",
	}, labelNames)
)

func init() {
	prometheus.MustRegister(poolCapacity)
	prometheus.MustRegister(poolActive)
}
