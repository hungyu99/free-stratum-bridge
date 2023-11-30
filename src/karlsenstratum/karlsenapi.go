package karlsenstratum

import (
	"context"
	"fmt"
	"time"

	"github.com/hungyu99/free-stratum-bridge/src/gostratum"
	"github.com/hungyu99/freed/app/appmessage"
	"github.com/hungyu99/freed/infrastructure/network/rpcclient"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

type KarlsenApi struct {
	address       string
	blockWaitTime time.Duration
	logger        *zap.SugaredLogger
	freed      *rpcclient.RPCClient
	connected     bool
}

func NewKarlsenAPI(address string, blockWaitTime time.Duration, logger *zap.SugaredLogger) (*KarlsenApi, error) {
	client, err := rpcclient.NewRPCClient(address)
	if err != nil {
		return nil, err
	}

	return &KarlsenApi{
		address:       address,
		blockWaitTime: blockWaitTime,
		logger:        logger.With(zap.String("component", "karlsenapi:"+address)),
		freed:      client,
		connected:     true,
	}, nil
}

func (ks *KarlsenApi) Start(ctx context.Context, blockCb func()) {
	ks.waitForSync(true)
	go ks.startBlockTemplateListener(ctx, blockCb)
	go ks.startStatsThread(ctx)
}

func (ks *KarlsenApi) startStatsThread(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	for {
		select {
		case <-ctx.Done():
			ks.logger.Warn("context cancelled, stopping stats thread")
			return
		case <-ticker.C:
			dagResponse, err := ks.freed.GetBlockDAGInfo()
			if err != nil {
				ks.logger.Warn("failed to get network hashrate from free, prom stats will be out of date", zap.Error(err))
				continue
			}
			response, err := ks.freed.EstimateNetworkHashesPerSecond(dagResponse.TipHashes[0], 1000)
			if err != nil {
				ks.logger.Warn("failed to get network hashrate from free, prom stats will be out of date", zap.Error(err))
				continue
			}
			RecordNetworkStats(response.NetworkHashesPerSecond, dagResponse.BlockCount, dagResponse.Difficulty)
		}
	}
}

func (ks *KarlsenApi) reconnect() error {
	if ks.freed != nil {
		return ks.freed.Reconnect()
	}

	client, err := rpcclient.NewRPCClient(ks.address)
	if err != nil {
		return err
	}
	ks.freed = client
	return nil
}

func (s *KarlsenApi) waitForSync(verbose bool) error {
	if verbose {
		s.logger.Info("checking freed sync state")
	}
	for {
		clientInfo, err := s.freed.GetInfo()
		if err != nil {
			return errors.Wrapf(err, "error fetching server info from freed @ %s", s.address)
		}
		if clientInfo.IsSynced {
			break
		}
		s.logger.Warn("free is not synced, waiting for sync before starting bridge")
		time.Sleep(5 * time.Second)
	}
	if verbose {
		s.logger.Info("freed synced, starting server")
	}
	return nil
}

func (s *KarlsenApi) startBlockTemplateListener(ctx context.Context, blockReadyCb func()) {
	blockReadyChan := make(chan bool)
	err := s.freed.RegisterForNewBlockTemplateNotifications(func(_ *appmessage.NewBlockTemplateNotificationMessage) {
		blockReadyChan <- true
	})
	if err != nil {
		s.logger.Error("fatal: failed to register for block notifications from free")
	}

	ticker := time.NewTicker(s.blockWaitTime)
	for {
		if err := s.waitForSync(false); err != nil {
			s.logger.Error("error checking freed sync state, attempting reconnect: ", err)
			if err := s.reconnect(); err != nil {
				s.logger.Error("error reconnecting to freed, waiting before retry: ", err)
				time.Sleep(5 * time.Second)
			}
		}
		select {
		case <-ctx.Done():
			s.logger.Warn("context cancelled, stopping block update listener")
			return
		case <-blockReadyChan:
			blockReadyCb()
			ticker.Reset(s.blockWaitTime)
		case <-ticker.C: // timeout, manually check for new blocks
			blockReadyCb()
		}
	}
}

func (ks *KarlsenApi) GetBlockTemplate(
	client *gostratum.StratumContext) (*appmessage.GetBlockTemplateResponseMessage, error) {
	template, err := ks.freed.GetBlockTemplate(client.WalletAddr,
		fmt.Sprintf(`'%s' via hungyu99/free-stratum-bridge_%s`, client.RemoteApp, version))
	if err != nil {
		return nil, errors.Wrap(err, "failed fetching new block template from free")
	}
	return template, nil
}
