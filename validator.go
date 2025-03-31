package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cosmos/cosmos-sdk/crypto/keys/ed25519"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	"github.com/cosmos/cosmos-sdk/types/bech32"
	slashing "github.com/cosmos/cosmos-sdk/x/slashing/types"
	staking "github.com/cosmos/cosmos-sdk/x/staking/types"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
)

// ValInfo holds most of the stats/info used for secondary alarms. It is refreshed roughly every minute.
type ValInfo struct {
	Moniker    string `json:"moniker"`
	Bonded     bool   `json:"bonded"`
	Jailed     bool   `json:"jailed"`
	Tombstoned bool   `json:"tombstoned"`
	Missed     int64  `json:"missed"`
	Window     int64  `json:"window"`
	Conspub    []byte `json:"conspub"`
	Valcons    string `json:"valcons"`
}

// GetValInfo the first bool is used to determine if extra information about the validator should be printed.
func (c *Config) GetValInfo(first bool) (err error) {
	if c.client == nil {
		return errors.New("nil rpc client")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if c.valInfo == nil {
		c.valInfo = &ValInfo{}
	}

	// Fetch info from /cosmos.staking.v1beta1.Query/Validator
	// it's easier to ask people to provide valoper since it's readily available on
	// explorers, so make it easy and lookup the consensus key for them.
	c.valInfo.Conspub, c.valInfo.Moniker, c.valInfo.Jailed, c.valInfo.Bonded, err = getVal(ctx, c.rpcClient, c.ValAddress)
	if err != nil {
		return err
	}
	if first && c.valInfo.Bonded {
		c.logger.Infof("⚙️ found %s (%s) in validator set", c.ValAddress, c.valInfo.Moniker)
	} else if first && !c.valInfo.Bonded {
		c.logger.Warningf("❌ %s (%s) is INACTIVE", c.ValAddress, c.valInfo.Moniker)
	}

	if strings.Contains(c.ValAddress, "valcons") {
		// no need to change prefix for signing info query
		c.valInfo.Valcons = c.ValAddress
	} else {
		// need to know the prefix for when we serialize the slashing info query, this is too fragile.
		// for now, we perform specific chain overrides based on known values because the valoper is used
		// in so many places.
		var prefix string
		split := strings.Split(c.ValAddress, "valoper")
		if len(split) != 2 {
			if pre, ok := altValopers.getAltPrefix(c.ValAddress); ok {
				c.valInfo.Valcons, err = bech32.ConvertAndEncode(pre, c.valInfo.Conspub[:20])
				if err != nil {
					return err
				}
			} else {
				err = errors.New("❓ could not determine bech32 prefix from valoper address: " + c.ValAddress)
				return err
			}
		} else {
			prefix = split[0] + "valcons"
			c.valInfo.Valcons, err = bech32.ConvertAndEncode(prefix, c.valInfo.Conspub[:20])
			if err != nil {
				return err
			}
		}
		if first {
			c.logger.Info("⚙️", c.ValAddress[:20], "... is using consensus key:", c.valInfo.Valcons)
		}

	}

	// get current signing information (tombstoned, missed block count)
	qSigning := slashing.QuerySigningInfoRequest{ConsAddress: c.valInfo.Valcons}
	b, err := qSigning.Marshal()
	if err != nil {
		return err
	}
	resp, err := c.rpcClient.ABCIQuery(ctx, "/cosmos.slashing.v1beta1.Query/SigningInfo", b)
	if resp == nil || resp.Response.Value == nil {
		err = errors.New("could not query validator slashing status, got empty response")
		return err
	}
	slash := &slashing.QuerySigningInfoResponse{}
	err = slash.Unmarshal(resp.Response.Value)
	if err != nil {
		return err
	}
	c.valInfo.Tombstoned = slash.ValSigningInfo.Tombstoned
	if c.valInfo.Tombstoned {
		c.logger.Errorf("❗️☠️ %s (%s) is tombstoned 🪦❗️", c.ValAddress, c.valInfo.Moniker)
	}
	c.valInfo.Missed = slash.ValSigningInfo.MissedBlocksCounter
	c.statsChan <- c.mkUpdate(metricWindowMissed, float64(c.valInfo.Missed), "")

	// finally get the signed blocks window
	if c.valInfo.Window == 0 {
		qParams := &slashing.QueryParamsRequest{}
		b, err = qParams.Marshal()
		if err != nil {
			return err
		}
		resp, err = c.rpcClient.ABCIQuery(ctx, "/cosmos.slashing.v1beta1.Query/Params", b)
		if err != nil {
			return err
		}
		if resp.Response.Value == nil {
			err = errors.New("🛑 could not query slashing params, got empty response")
			return err
		}
		params := &slashing.QueryParamsResponse{}
		err = params.Unmarshal(resp.Response.Value)
		if err != nil {
			return err
		}
		if first {
			c.statsChan <- c.mkUpdate(metricWindowSize, float64(params.Params.SignedBlocksWindow), "")
			c.statsChan <- c.mkUpdate(metricTotalNodes, float64(len(c.Nodes)), "")
		}
		c.valInfo.Window = params.Params.SignedBlocksWindow
	}
	return
}

// getVal returns the public key, moniker, and if the validator is jailed.
func getVal(ctx context.Context, client *rpchttp.HTTP, valoper string) (pub []byte, moniker string, jailed, bonded bool, err error) {
	if strings.Contains(valoper, "valcons") {
		_, bz, err := bech32.DecodeAndConvert(valoper)
		if err != nil {
			return nil, "", false, false, errors.New("could not decode and convert your address" + valoper)
		}

		hexAddress := fmt.Sprintf("%X", bz)
		return ToBytes(hexAddress), valoper, false, true, nil
	}

	q := staking.QueryValidatorRequest{
		ValidatorAddr: valoper,
	}
	b, err := q.Marshal()
	if err != nil {
		return
	}
	resp, err := client.ABCIQuery(ctx, "/cosmos.staking.v1beta1.Query/Validator", b)
	if err != nil {
		return
	}
	if resp.Response.Value == nil {
		return nil, "", false, false, errors.New("could not find validator " + valoper)
	}
	val := &staking.QueryValidatorResponse{}
	err = val.Unmarshal(resp.Response.Value)
	if err != nil {
		return
	}
	if val.Validator.ConsensusPubkey == nil {
		return nil, "", false, false, errors.New("got invalid consensus pubkey for " + valoper)
	}

	pubBytes := make([]byte, 0)
	switch val.Validator.ConsensusPubkey.TypeUrl {
	case "/cosmos.crypto.ed25519.PubKey":
		pk := ed25519.PubKey{}
		err = pk.Unmarshal(val.Validator.ConsensusPubkey.Value)
		if err != nil {
			return
		}
		pubBytes = pk.Address().Bytes()
	case "/cosmos.crypto.secp256k1.PubKey":
		pk := secp256k1.PubKey{}
		err = pk.Unmarshal(val.Validator.ConsensusPubkey.Value)
		if err != nil {
			return
		}
		pubBytes = pk.Address().Bytes()
	}
	if len(pubBytes) == 0 {
		return nil, "", false, false, errors.New("could not get pubkey for" + valoper)
	}

	return pubBytes, val.Validator.GetMoniker(), val.Validator.Jailed, val.Validator.Status == 3, nil
}

func ToBytes(address string) []byte {
	bz, _ := hex.DecodeString(strings.ToLower(address))
	return bz
}
