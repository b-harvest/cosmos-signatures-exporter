package main

import (
	"context"
	log "github.com/sirupsen/logrus"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	"sync"
	"time"
)

type Config struct {
	ctx    context.Context
	cancel context.CancelFunc

	logger *log.Entry

	PrometheusListenPort int `yaml:"prometheusListenPort"`

	statsChan chan *promUpdate

	queryInterval time.Duration

	name           string        `yaml:"name"`
	client         *rpchttp.HTTP // legit tendermint client
	noNodes        bool          // tracks if all nodes are down
	valInfo        *ValInfo      // recent validator state, only refreshed every few minutes
	lastValInfo    *ValInfo      // use for detecting newly-jailed/tombstone
	blocksResults  []int
	lastError      string
	lastBlockTime  time.Time
	lastBlockAlarm bool
	lastBlockMtx   *sync.Mutex
	lastBlockNum   int64
	activeAlerts   int

	statTotalSigns      float64
	statTotalProps      float64
	statTotalMiss       float64
	statConsecutiveMiss float64

	ChainId    string `yaml:"chain_id"`
	ValAddress string `yaml:"valoper_address"`
	ExtraInfo  string `yaml:"extra_info"`

	Nodes []*NodeConfig `yaml:"nodes"`

	Healthcheck HealthcheckConfig `yaml:"healthcheck"`

	PublicFallback bool `yaml:"public_fallback"`
}

// NodeConfig holds the basic information for a node to connect to.
type NodeConfig struct {
	Url         string `yaml:"url"`
	AlertIfDown bool   `yaml:"alert_if_down"`

	down      bool
	wasDown   bool
	syncing   bool
	lastMsg   string
	downSince time.Time
}

type HealthcheckConfig struct {
	Enabled  bool          `yaml:"enabled"`
	PingURL  string        `yaml:"ping_url"`
	PingRate time.Duration `yaml:"ping_rate"`
}

func (c *Config) mkUpdate(t metricType, v float64, node string) *promUpdate {
	return &promUpdate{
		metric:   t,
		counter:  v,
		name:     c.name,
		chainId:  c.ChainId,
		moniker:  c.valInfo.Moniker,
		endpoint: node,
	}
}
