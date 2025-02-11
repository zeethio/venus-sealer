package sealing

import (
	"bytes"
	"context"
	"sort"
	"sync"
	"time"

	"github.com/filecoin-project/venus/pkg/specactors/builtin/miner"
	"github.com/filecoin-project/venus/pkg/specactors/policy"

	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	miner0 "github.com/filecoin-project/specs-actors/actors/builtin/miner"
	miner5 "github.com/filecoin-project/specs-actors/v5/actors/builtin/miner"

	"github.com/filecoin-project/venus-sealer/api"
	"github.com/filecoin-project/venus-sealer/config"
	"github.com/filecoin-project/venus-sealer/storage-sealing/sealiface"
	"github.com/filecoin-project/venus-sealer/types"
)

//go:generate go run github.com/golang/mock/mockgen -destination=mocks/mock_precommit_batcher.go -package=mocks . PreCommitBatcherApi

type PreCommitBatcherApi interface {
	//for messager
	MessagerSendMsg(ctx context.Context, from, to address.Address, method abi.MethodNum, value, maxFee abi.TokenAmount, params []byte) (string, error)

	StateMinerInfo(context.Context, address.Address, types.TipSetToken) (miner.MinerInfo, error)
	ChainHead(ctx context.Context) (types.TipSetToken, abi.ChainEpoch, error)
}

type preCommitEntry struct {
	deposit abi.TokenAmount
	pci     *miner0.SectorPreCommitInfo
}

type PreCommitBatcher struct {
	api       PreCommitBatcherApi
	maddr     address.Address
	mctx      context.Context
	addrSel   AddrSel
	feeCfg    config.MinerFeeConfig
	getConfig types.GetSealingConfigFunc

	cutoffs map[abi.SectorNumber]time.Time
	todo    map[abi.SectorNumber]*preCommitEntry
	waiting map[abi.SectorNumber][]chan sealiface.PreCommitBatchRes

	notify, stop, stopped chan struct{}
	force                 chan chan []sealiface.PreCommitBatchRes
	lk                    sync.Mutex

	networkParams *config.NetParamsConfig
}

func NewPreCommitBatcher(mctx context.Context, networkParams *config.NetParamsConfig, maddr address.Address, api PreCommitBatcherApi, addrSel AddrSel, feeCfg config.MinerFeeConfig, getConfig types.GetSealingConfigFunc) *PreCommitBatcher {
	b := &PreCommitBatcher{
		api:       api,
		maddr:     maddr,
		mctx:      mctx,
		addrSel:   addrSel,
		feeCfg:    feeCfg,
		getConfig: getConfig,

		cutoffs: map[abi.SectorNumber]time.Time{},
		todo:    map[abi.SectorNumber]*preCommitEntry{},
		waiting: map[abi.SectorNumber][]chan sealiface.PreCommitBatchRes{},

		notify:  make(chan struct{}, 1),
		force:   make(chan chan []sealiface.PreCommitBatchRes),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),

		networkParams: networkParams,
	}

	go b.run()

	return b
}

func (b *PreCommitBatcher) run() {
	var forceRes chan []sealiface.PreCommitBatchRes
	var lastRes []sealiface.PreCommitBatchRes

	cfg, err := b.getConfig()
	if err != nil {
		panic(err)
	}

	timer := time.NewTimer(b.batchWait(cfg.PreCommitBatchWait, cfg.PreCommitBatchSlack))
	for {
		if forceRes != nil {
			forceRes <- lastRes
			forceRes = nil
		}
		lastRes = nil

		var sendAboveMax bool
		select {
		case <-b.stop:
			close(b.stopped)
			return
		case <-b.notify:
			sendAboveMax = true
		case <-timer.C:
			// do nothing
		case fr := <-b.force: // user triggered
			forceRes = fr
		}

		var err error
		lastRes, err = b.maybeStartBatch(sendAboveMax)
		if err != nil {
			log.Warnw("PreCommitBatcher processBatch error", "error", err)
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}

		timer.Reset(b.batchWait(cfg.PreCommitBatchWait, cfg.PreCommitBatchSlack))
	}
}

func (b *PreCommitBatcher) batchWait(maxWait, slack time.Duration) time.Duration {
	now := time.Now()

	b.lk.Lock()
	defer b.lk.Unlock()

	if len(b.todo) == 0 {
		return maxWait
	}

	var cutoff time.Time
	for sn := range b.todo {
		sectorCutoff := b.cutoffs[sn]
		if cutoff.IsZero() || (!sectorCutoff.IsZero() && sectorCutoff.Before(cutoff)) {
			cutoff = sectorCutoff
		}
	}
	for sn := range b.waiting {
		sectorCutoff := b.cutoffs[sn]
		if cutoff.IsZero() || (!sectorCutoff.IsZero() && sectorCutoff.Before(cutoff)) {
			cutoff = sectorCutoff
		}
	}

	if cutoff.IsZero() {
		return maxWait
	}

	cutoff = cutoff.Add(-slack)
	if cutoff.Before(now) {
		return time.Nanosecond // can't return 0
	}

	wait := cutoff.Sub(now)
	if wait > maxWait {
		wait = maxWait
	}

	return wait
}

func (b *PreCommitBatcher) maybeStartBatch(notif bool) ([]sealiface.PreCommitBatchRes, error) {
	b.lk.Lock()
	defer b.lk.Unlock()

	total := len(b.todo)
	if total == 0 {
		return nil, nil // nothing to do
	}

	cfg, err := b.getConfig()
	if err != nil {
		return nil, xerrors.Errorf("getting config: %w", err)
	}

	if notif && total < cfg.MaxPreCommitBatch {
		return nil, nil
	}

	// todo support multiple batches
	res, err := b.processBatch(cfg)
	if err != nil && len(res) == 0 {
		return nil, err
	}

	for _, r := range res {
		if err != nil {
			r.Error = err.Error()
		}

		for _, sn := range r.Sectors {
			for _, ch := range b.waiting[sn] {
				ch <- r // buffered
			}

			delete(b.waiting, sn)
			delete(b.todo, sn)
			delete(b.cutoffs, sn)
		}
	}

	return res, nil
}

func (b *PreCommitBatcher) processBatch(cfg sealiface.Config) ([]sealiface.PreCommitBatchRes, error) {
	params := miner5.PreCommitSectorBatchParams{}
	deposit := big.Zero()
	var res sealiface.PreCommitBatchRes

	for _, p := range b.todo {
		if len(params.Sectors) >= cfg.MaxPreCommitBatch {
			log.Infow("precommit batch full")
			break
		}

		res.Sectors = append(res.Sectors, p.pci.SectorNumber)
		params.Sectors = append(params.Sectors, *p.pci)
		deposit = big.Add(deposit, p.deposit)
	}

	enc := new(bytes.Buffer)
	if err := params.MarshalCBOR(enc); err != nil {
		return []sealiface.PreCommitBatchRes{res}, xerrors.Errorf("couldn't serialize PreCommitSectorBatchParams: %w", err)
	}

	mi, err := b.api.StateMinerInfo(b.mctx, b.maddr, nil)
	if err != nil {
		return []sealiface.PreCommitBatchRes{res}, xerrors.Errorf("couldn't get miner info: %w", err)
	}

	maxFee := b.feeCfg.MaxPreCommitBatchGasFee.FeeForSectors(len(params.Sectors))
	goodFunds := big.Add(deposit, maxFee)

	from, _, err := b.addrSel(b.mctx, mi, api.PreCommitAddr, goodFunds, deposit)
	if err != nil {
		return []sealiface.PreCommitBatchRes{res}, xerrors.Errorf("no good address found: %w", err)
	}

	uid, err := b.api.MessagerSendMsg(b.mctx, from, b.maddr, miner.Methods.PreCommitSectorBatch, deposit, maxFee, enc.Bytes())
	if err != nil {
		return []sealiface.PreCommitBatchRes{res}, xerrors.Errorf("sending message failed: %w", err)
	}

	res.Msg = uid

	log.Infow("Sent PreCommitSectorBatch message", "uid", uid, "from", from, "sectors", len(b.todo))

	return []sealiface.PreCommitBatchRes{res}, nil
}

// register PreCommit, wait for batch message, return message CID
func (b *PreCommitBatcher) AddPreCommit(ctx context.Context, s types.SectorInfo, deposit abi.TokenAmount, in *miner0.SectorPreCommitInfo) (res sealiface.PreCommitBatchRes, err error) {
	_, curEpoch, err := b.api.ChainHead(b.mctx)
	if err != nil {
		log.Errorf("getting chain head: %s", err)
		return sealiface.PreCommitBatchRes{}, err
	}

	sn := s.SectorNumber

	b.lk.Lock()
	b.cutoffs[sn] = getPreCommitCutoff(curEpoch, s, b.networkParams.BlockDelaySecs)
	b.todo[sn] = &preCommitEntry{
		deposit: deposit,
		pci:     in,
	}

	sent := make(chan sealiface.PreCommitBatchRes, 1)
	b.waiting[sn] = append(b.waiting[sn], sent)

	select {
	case b.notify <- struct{}{}:
	default: // already have a pending notification, don't need more
	}
	b.lk.Unlock()

	select {
	case c := <-sent:
		return c, nil
	case <-ctx.Done():
		return sealiface.PreCommitBatchRes{}, ctx.Err()
	}
}

func (b *PreCommitBatcher) Flush(ctx context.Context) ([]sealiface.PreCommitBatchRes, error) {
	resCh := make(chan []sealiface.PreCommitBatchRes, 1)
	select {
	case b.force <- resCh:
		select {
		case res := <-resCh:
			return res, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *PreCommitBatcher) Pending(ctx context.Context) ([]abi.SectorID, error) {
	b.lk.Lock()
	defer b.lk.Unlock()

	mid, err := address.IDFromAddress(b.maddr)
	if err != nil {
		return nil, err
	}

	res := make([]abi.SectorID, 0)
	for _, s := range b.todo {
		res = append(res, abi.SectorID{
			Miner:  abi.ActorID(mid),
			Number: s.pci.SectorNumber,
		})
	}

	sort.Slice(res, func(i, j int) bool {
		if res[i].Miner != res[j].Miner {
			return res[i].Miner < res[j].Miner
		}

		return res[i].Number < res[j].Number
	})

	return res, nil
}

func (b *PreCommitBatcher) Stop(ctx context.Context) error {
	close(b.stop)

	select {
	case <-b.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TODO: If this returned epochs, it would make testing much easier
func getPreCommitCutoff(curEpoch abi.ChainEpoch, si types.SectorInfo, blockDelaySecs uint64) time.Time {
	cutoffEpoch := si.TicketEpoch + policy.MaxPreCommitRandomnessLookback
	for _, p := range si.Pieces {
		if p.DealInfo == nil {
			continue
		}

		startEpoch := p.DealInfo.DealSchedule.StartEpoch
		if startEpoch < cutoffEpoch {
			cutoffEpoch = startEpoch
		}
	}

	if cutoffEpoch <= curEpoch {
		return time.Now()
	}

	return time.Now().Add(time.Duration(cutoffEpoch-curEpoch) * time.Duration(blockDelaySecs) * time.Second)
}
