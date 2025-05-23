package main

import (
	"context"
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"net/http"
	"sync"
	"time"
)

var (
	promMux sync.RWMutex
)

type metricType uint8

const (
	metricSigned metricType = iota
	metricProposed
	metricMissed
	metricConsecutive
	metricWindowMissed
	metricWindowSize
	metricLastBlockSeconds
	metricLastBlockSecondsNotFinal

	metricTotalNodes
	metricUnealthyNodes
	metricNodeLagSeconds
	metricNodeDownSeconds
)

type promUpdate struct {
	metric   metricType
	counter  float64
	name     string
	chainId  string
	moniker  string
	endpoint string
}

type metrics map[metricType]*prometheus.GaugeVec

func (m metrics) setStat(update *promUpdate) {
	lbls := map[string]string{
		"name":     update.name,
		"chain_id": update.chainId,
		"moniker":  update.moniker,
	}
	promMux.RLock()
	defer promMux.RUnlock()
	if update.metric == metricNodeLagSeconds || update.metric == metricNodeDownSeconds {
		lbls["endpoint"] = update.endpoint
	}
	m[update.metric].With(lbls).Set(update.counter)
}

func prometheusExporter(l *log.Entry, ctx context.Context, updates chan *promUpdate) {
	// attributes used to uniquely identify each chain
	var chainLabels = []string{"name", "chain_id", "moniker"}
	var hostLabels = []string{"name", "chain_id", "moniker", "endpoint"}

	// setup our signing gauges
	signed := promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cosmos_sigs_signed_blocks",
		Help: "count of blocks signed since cosmos_sig was started",
	}, chainLabels)
	proposed := promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cosmos_sigs_proposed_blocks",
		Help: "count of blocks proposed since cosmos_sig was started",
	}, chainLabels)
	missed := promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cosmos_sigs_missed_blocks",
		Help: "count of blocks missed without seeing a precommit or prevote since cosmos_sig was started",
	}, chainLabels)
	missedConsecutive := promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cosmos_sigs_consecutive_missed_blocks",
		Help: "the current count of consecutively missed blocks regardless of precommit or prevote status",
	}, chainLabels)
	windowSize := promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cosmos_sigs_missed_block_window",
		Help: "the missed block aka slashing window",
	}, chainLabels)
	missedWindow := promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cosmos_sigs_missed_blocks_for_window",
		Help: "the current count of missed blocks in the slashing window regardless of precommit or prevote status",
	}, chainLabels)
	lastBlockSec := promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cosmos_sigs_time_since_last_block",
		Help: "how many seconds since the previous block was finalized, only set when a new block is seen, not useful for stall detection, helpful for averaging times",
	}, chainLabels)
	lastBlockSecUnfinalized := promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cosmos_sigs_time_since_last_block_unfinalized",
		Help: "how many seconds since the previous block was finalized, set regardless of finalization, useful for stall detection, not helpful for figuring average time",
	}, chainLabels)

	// setup node health gauges:
	nodesMonitored := promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cosmos_sigs_total_monitored_endpoints",
		Help: "the count of rpc endpoints being monitored for a chain",
	}, chainLabels)
	nodesUnhealthy := promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cosmos_sigs_total_unhealthy_endpoints",
		Help: "the count of unhealthy rpc endpoints being monitored for a chain",
	}, chainLabels)

	// extra labels for individual node stats
	nodeLagSec := promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cosmos_sigs_endpoint_syncing_seconds_behind",
		Help: "how many seconds a node is behind the head of a chain",
	}, hostLabels)
	nodeDownSec := promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cosmos_sigs_endpoint_down_seconds",
		Help: "how many seconds a node has been marked as unhealthy",
	}, hostLabels)

	m := metrics{
		metricSigned:                   signed,
		metricProposed:                 proposed,
		metricMissed:                   missed,
		metricConsecutive:              missedConsecutive,
		metricWindowMissed:             missedWindow,
		metricWindowSize:               windowSize,
		metricLastBlockSeconds:         lastBlockSec,
		metricLastBlockSecondsNotFinal: lastBlockSecUnfinalized,
		metricTotalNodes:               nodesMonitored,
		metricUnealthyNodes:            nodesUnhealthy,
		metricNodeLagSeconds:           nodeLagSec,  // todo
		metricNodeDownSeconds:          nodeDownSec, // todo
	}

	go func() {
		for {
			select {
			case u := <-updates:
				m.setStat(u)
			case <-ctx.Done():
				return
			}
		}
	}()

	promMux := http.NewServeMux()

	l.Infof("serving prometheus metrics at 0.0.0.0:%d/metrics", cfg.PrometheusListenPort)
	promMux.Handle("/metrics", promhttp.Handler())
	promSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.PrometheusListenPort),
		Handler:           promMux,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       120 * time.Second,
		ReadHeaderTimeout: 20 * time.Second,
	}
	log.Fatal(promSrv.ListenAndServe())
}
