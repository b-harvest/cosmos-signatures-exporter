package main

import (
	"context"
	"gopkg.in/yaml.v3"
	"os"
	"sync"
	"time"
)

func loadConfig(configPath string) error {
	var (
		err error
		b   []byte
	)

	b, err = os.ReadFile(configPath)
	if err = yaml.Unmarshal(b, &cfg); err != nil {
		return err
	}
	if cfg.queryInterval == 0 {
		cfg.queryInterval = time.Second * 3
	}

	cfg.ctx, cfg.cancel = context.WithCancel(context.Background())
	cfg.statsChan = make(chan *promUpdate, 2)
	cfg.lastBlockMtx = &sync.Mutex{}

	return nil
}
