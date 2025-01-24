package main

import (
	"flag"
	log "github.com/sirupsen/logrus"
	"os"
	"time"
)

var (
	cfg *Config
)

func init() {
	cfg = &Config{}
	time.Local = time.UTC

	var (
		logLevel   string
		configFile string
		l          log.Level
		err        error
	)

	flag.StringVar(&logLevel, "log-level", "info", "allow showing debug log")
	flag.StringVar(&configFile, "config", "", "configuration file path")
	flag.Parse()
	if logLevel == "" && os.Getenv("LOG_LEVEL") != "" {
		logLevel = os.Getenv("LOG_LEVEL")
	} else {
		logLevel = "info"
	}

	if configFile == "" {
		if os.Getenv("CONFIG_PATH") != "" {
			configFile = os.Getenv("CONFIG_PATH")
		} else {
			configFile = "./config.yaml"
		}
	}

	l, err = log.ParseLevel(logLevel)
	if err != nil {
		panic(err)
	}
	log.SetLevel(l)

	cfg.logger = log.NewEntry(log.StandardLogger())

	if err = loadConfig(configFile); err != nil {
		panic(err)
	}
}

func main() {

	go prometheusExporter(cfg.logger, cfg.ctx, cfg.statsChan)

	go func() {
		go func() {
			for {
				cfg.monitorHealth(cfg.ctx, cfg.name)
			}
		}()

		// websocket subscription and occasional validator info refreshes
		for {
			e := cfg.newRpc()
			if e != nil {
				cfg.logger.Errorf(cfg.ChainId, e)
				time.Sleep(5 * time.Second)
				continue
			}

			e = cfg.GetValInfo(true)
			if e != nil {
				cfg.logger.Error("🛑", cfg.ChainId, e)
			}

			cfg.RunBlockQuerier()
			cfg.logger.Errorf(cfg.ChainId, "🌀 tendermint client exited! Restarting monitoring")
			time.Sleep(5 * time.Second)
		}
	}()

	<-cfg.ctx.Done()
}
