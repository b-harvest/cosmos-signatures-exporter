package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	jsonrpcclient "github.com/tendermint/tendermint/rpc/jsonrpc/client"
)

// newRpc sets up the rpc client used for monitoring. It will try nodes in order until a working node is found.
// it will also get some initial info on the validator's status.
func (c *Config) newRpc() error {
	_, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var anyWorking bool // if healthchecks are running, we will skip to the first known good node.
	for _, endpoint := range c.Nodes {
		anyWorking = anyWorking || !endpoint.down
	}
	// grab the first working endpoint
	tryUrl := func(u string) (msg string, down, syncing bool) {
		_, err := url.Parse(u)
		if err != nil {
			msg = fmt.Sprintf("❌ could not parse url %s: (%s) %s", c.name, u, err)
			c.logger.Warning(msg)
			down = true
			return
		}
		c.client, err = jsonrpcclient.NewURI(u)
		c.rpcClient, err = rpchttp.New(u)
		if err != nil {
			msg = fmt.Sprintf("❌ could not connect client for %s: (%s) %s", c.name, u, err)
			c.logger.Warningf(msg)
			down = true
			return
		}
		//status, err := c.client.Status(ctx)
		//if err != nil {
		//	msg = fmt.Sprintf("❌ could not get status for %s: (%s) %s", c.name, u, err)
		//	down = true
		//	c.logger.Warning(msg)
		//	return
		//}
		//if status.NodeInfo.Network != c.ChainId {
		//	msg = fmt.Sprintf("chain id %s on %s does not match, expected %s, skipping", status.NodeInfo.Network, u, c.ChainId)
		//	down = true
		//	c.logger.Warning(msg)
		//	return
		//}
		//if status.SyncInfo.CatchingUp {
		//	msg = fmt.Sprint("🐢 node is not synced, skipping ", u)
		//	syncing = true
		//	down = true
		//	c.logger.Warning(msg)
		//	return
		//}
		c.noNodes = false
		return
	}
	down := func(endpoint *NodeConfig, msg string) {
		if !endpoint.down {
			endpoint.down = true
			endpoint.downSince = time.Now()
		}
		endpoint.lastMsg = msg
	}
	for _, endpoint := range c.Nodes {
		if anyWorking && endpoint.down {
			continue
		}
		if msg, failed, syncing := tryUrl(endpoint.Url); failed {
			endpoint.syncing = syncing
			down(endpoint, msg)
			continue
		}
		return nil
	}
	if c.PublicFallback {
		if u, ok := getRegistryUrl(c.ChainId); ok {
			node := guessPublicEndpoint(u)
			c.logger.Warning(c.ChainId, "⛑ attemtping to use public fallback node", node)
			if _, kk, _ := tryUrl(node); !kk {
				c.logger.Info(c.ChainId, "⛑ connected to public endpoint", node)
				return nil
			}
		} else {
			c.logger.Warning("could not find a public endpoint for", c.ChainId)
		}
	}
	c.noNodes = true
	c.lastError = "no usable RPC endpoints available for " + c.ChainId
	return errors.New("no usable endpoints available for " + c.ChainId)
}

func (c *Config) monitorHealth(ctx context.Context, chainName string) {
	tick := time.NewTicker(time.Minute)
	if c.client == nil {
		_ = c.newRpc()
	}

	for {
		select {
		case <-ctx.Done():
			return

		case <-tick.C:
			var err error
			for _, node := range c.Nodes {
				go func(node *NodeConfig) {
					alert := func(msg string) {
						node.lastMsg = fmt.Sprintf("%-12s node %s is %s", chainName, node.Url, msg)
						if !node.AlertIfDown {
							// even if we aren't alerting, we want to display the status in the dashboard.
							node.down = true
							return
						}
						if !node.down {
							node.down = true
							node.downSince = time.Now()
						}
						c.statsChan <- c.mkUpdate(metricNodeDownSeconds, time.Since(node.downSince).Seconds(), node.Url)

						c.logger.Warning("⚠️ " + node.lastMsg)
					}
					client, e := rpchttp.New(node.Url)
					if e != nil {
						alert(e.Error())
					}
					cwt, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					status, e := client.Status(cwt)
					cancel()
					if e != nil {
						alert("down")
						return
					}
					if status.NodeInfo.Network != c.ChainId {
						alert("on the wrong network")
						return
					}
					if status.SyncInfo.CatchingUp {
						alert("not synced")
						node.syncing = true
						return
					}

					// node's OK, clear the note
					if node.down {
						node.lastMsg = ""
						node.wasDown = true
					}
					c.statsChan <- c.mkUpdate(metricNodeDownSeconds, 0, node.Url)
					node.down = false
					node.syncing = false
					node.downSince = time.Unix(0, 0)
					c.noNodes = false
					c.logger.Info(fmt.Sprintf("🟢 %-12s node %s is healthy", chainName, node.Url))
				}(node)
			}

			if c.client == nil {
				e := c.newRpc()
				if e != nil {
					c.logger.Warning("💥", c.ChainId, e)
				}
			}
			if c.valInfo != nil {
				c.lastValInfo = &ValInfo{
					Moniker:    c.valInfo.Moniker,
					Bonded:     c.valInfo.Bonded,
					Jailed:     c.valInfo.Jailed,
					Tombstoned: c.valInfo.Tombstoned,
					Missed:     c.valInfo.Missed,
					Window:     c.valInfo.Window,
					Conspub:    c.valInfo.Conspub,
					Valcons:    c.valInfo.Valcons,
				}
			}
			err = c.GetValInfo(false)
			if err != nil {
				c.logger.Warning("❓ refreshing signing info for", c.ValAddress, err)
			}
		}
	}
}

func (c *Config) pingHealthcheck() {
	if !c.Healthcheck.Enabled {
		return
	}

	ticker := time.NewTicker(c.Healthcheck.PingRate * time.Second)

	go func() {
		for {
			select {
			case <-ticker.C:
				_, err := http.Get(c.Healthcheck.PingURL)
				if err != nil {
					c.logger.Warning(fmt.Sprintf("❌ Failed to ping healthcheck URL: %s", err.Error()))
				} else {
					c.logger.Warning(fmt.Sprintf("🏓 Sucessfully pinged healthcheck URL: %s", c.Healthcheck.PingURL))
				}
			}
		}
	}()
}

// endpointRex matches the first a tag's hostname and port if present.
var endpointRex = regexp.MustCompile(`//([^/:]+)(:\d+)?`)

// guessPublicEndpoint attempts to deal with a shortcoming in the tendermint RPC client that doesn't allow path prefixes.
// The cosmos.directory requires them. This is a workaround to get the actual URL for the server behind their proxy.
// The RPC base URL will return links endpoints, and we can parse this to guess the original URL.
func guessPublicEndpoint(u string) string {
	resp, err := http.Get(u + "/")
	if err != nil {
		return u
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return u
	}
	_ = resp.Body.Close()
	matches := endpointRex.FindStringSubmatch(string(b))
	if len(matches) < 2 {
		// didn't work
		return u
	}
	proto := "https://"
	port := ":443"
	// will be 3 elements if there is a port no port means listening on https
	if len(matches) == 3 && matches[2] != "" && matches[2] != ":443" {
		proto = "http://"
		port = matches[2]
	}
	return proto + matches[1] + port
}
