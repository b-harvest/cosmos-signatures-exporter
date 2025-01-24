package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"github.com/pkg/errors"
	"github.com/tendermint/tendermint/rpc/coretypes"
	"strings"
	"time"
)

// StatusType represents the various possible end states. Prevote and Precommit are special cases, where the node
// monitoring for misses did see them, but the proposer did not include in the block.
type StatusType int

const (
	Statusmissed StatusType = iota
	StatusSigned
	StatusProposed
)

// StatusUpdate is passed over a channel from the websocket client indicating the current state, it is immediate in the
// case of prevotes etc, and the highest value seen is used in the final determination (which is how we tag
// prevote/precommit + missed blocks.
type StatusUpdate struct {
	Height int64
	Status StatusType
	Final  bool
}

func (c *Config) RunBlockQuerier() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := time.Now()
	for {
		// wait until our RPC client is connected and running. We will use the same URL for the websocket
		if c.client == nil || c.valInfo == nil || c.valInfo.Conspub == nil {
			if started.Before(time.Now().Add(-2 * time.Minute)) {
				c.logger.Info(c.name, "websocket client timed out waiting for a working rpc endpoint, restarting")
				return
			}
			c.logger.Info("⏰ waiting for a healthy client for", c.ChainId)
			time.Sleep(30 * time.Second)
			continue
		}
		break
	}

	c.logger.Info(c.name, "starting block querier")

	resultChan := make(chan StatusUpdate)
	go func() {
		var signState StatusType = -1
		for {
			select {
			case update := <-resultChan:
				if update.Final && update.Height%20 == 0 {
					c.logger.Info(fmt.Sprintf("🧊 %-12s block %d", c.ChainId, update.Height))
				}
				if update.Status > signState && c.valInfo.Bonded {
					signState = update.Status
				}
				if update.Final {
					c.lastBlockNum = update.Height
					c.statsChan <- c.mkUpdate(metricLastBlockSeconds, time.Since(c.lastBlockTime).Seconds(), "")

					c.lastBlockTime = time.Now()
					c.lastBlockAlarm = false
					c.blocksResults = []int{int(signState)}
					if len(c.blocksResults) != 0 {
						c.blocksResults = append([]int{int(signState)}, c.blocksResults[:len(c.blocksResults)-1]...)
					}
					if signState < StatusSigned && c.valInfo.Bonded {
						warn := fmt.Sprintf("❌ warning      %s missed block %d on %s", c.valInfo.Moniker, update.Height, c.ChainId)
						c.logger.Warning(warn)
					}
					switch signState {
					case Statusmissed:
						c.statTotalMiss += 1
						c.statConsecutiveMiss += 1
					case StatusSigned:
						c.statTotalSigns += 1
						c.statConsecutiveMiss = 0
					case StatusProposed:
						c.statTotalProps += 1
						c.statTotalSigns += 1
						c.statConsecutiveMiss = 0
					}
					signState = -1
					healthyNodes := 0
					for i := range c.Nodes {
						if !c.Nodes[i].down {
							healthyNodes += 1
						}
					}
					switch {
					case c.valInfo.Tombstoned:
						c.logger.Errorf("validator is tombstoned")
					case c.valInfo.Jailed:
						c.logger.Errorf("validator is jailed")
					}

					c.statsChan <- c.mkUpdate(metricSigned, c.statTotalSigns, "")
					c.statsChan <- c.mkUpdate(metricProposed, c.statTotalProps, "")
					c.statsChan <- c.mkUpdate(metricMissed, c.statTotalMiss, "")
					c.statsChan <- c.mkUpdate(metricConsecutive, c.statConsecutiveMiss, "")
					c.statsChan <- c.mkUpdate(metricUnealthyNodes, float64(len(c.Nodes)-healthyNodes), "")
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	blockChan := make(chan *coretypes.ResultBlock)
	go func() {
		e := handleBlocks(ctx, blockChan, resultChan, strings.ToUpper(hex.EncodeToString(c.valInfo.Conspub)))
		if e != nil {
			c.logger.Error("🛑", c.ChainId, e)
			cancel()
		}
	}()

	// now that channel consumers are up, create our subscriptions and route data.
	go func() {
		for {
			if c.lastBlockNum == 0 {
				tmStatus, err := c.client.Status(ctx)
				if err != nil {
					c.logger.Error(err)
					cancel()
					return
				}
				c.lastBlockMtx.Lock()
				c.lastBlockNum = tmStatus.SyncInfo.LatestBlockHeight - 1
				c.lastBlockMtx.Unlock()
				c.logger.Infof("lastBlockNum is 0. set to %d", c.lastBlockNum)
			}

			tmStatus, err := c.client.Status(ctx)
			if err != nil {
				c.logger.Error(err)
				cancel()
				return
			}

			c.lastBlockMtx.Lock()
			for ; c.lastBlockNum < tmStatus.SyncInfo.LatestBlockHeight-1; c.lastBlockNum += 1 {
				msg, e := c.client.Block(ctx, &c.lastBlockNum)
				if e != nil {
					c.logger.Error(e)
					cancel()
					return
				}
				c.logger.Infof("new block for %d", c.lastBlockNum)

				blockChan <- msg
				time.Sleep(100 * time.Millisecond)
			}
			c.lastBlockMtx.Unlock()

			time.Sleep(cfg.queryInterval)
		}
	}()

	c.logger.Info(fmt.Sprintf("⚙️ %-12s watching for NewBlock events via %s", c.ChainId, c.client.Remote()))
	for {
		select {
		case <-c.client.Quit():
			cancel()
		case <-ctx.Done():
			return
		}
	}
}

// handleBlocks consumes the channel for new blocks and when it sees one sends a status update. It's also
// responsible for stalled chain detection and will shutdown the client if there are no blocks for a minute.
func handleBlocks(ctx context.Context, blocks chan *coretypes.ResultBlock, results chan StatusUpdate, address string) error {
	live := time.NewTicker(time.Minute)
	defer live.Stop()
	lastBlock := time.Now()
	for {
		select {
		case <-live.C:
			// no block for a full minute likely means we have either a dead chain, or a dead client.
			if lastBlock.Before(time.Now().Add(-time.Minute)) {
				return errors.New("websocket idle for 1 minute, exiting")
			}
		case block := <-blocks:
			lastBlock = time.Now()
			upd := StatusUpdate{
				Height: block.Block.Header.Height,
				Status: Statusmissed,
				Final:  true,
			}
			if block.Block.Header.ProposerAddress.String() == address {
				upd.Status = StatusProposed
			} else {
				for _, sig := range block.Block.LastCommit.Signatures {
					if sig.ValidatorAddress.String() == address {
						upd.Status = StatusSigned
						break
					}
				}
			}
			results <- upd
		case <-ctx.Done():
			return nil
		}
	}
}
