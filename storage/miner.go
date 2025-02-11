package storage

import (
	"context"
	"errors"
	"time"

	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-bitfield"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/crypto"
	"github.com/filecoin-project/go-state-types/dline"
	"github.com/filecoin-project/go-state-types/network"
	"github.com/filecoin-project/specs-storage/storage"

	proof2 "github.com/filecoin-project/specs-actors/v2/actors/runtime/proof"

	"github.com/filecoin-project/venus/app/submodule/apitypes"
	"github.com/filecoin-project/venus/pkg/chain"
	"github.com/filecoin-project/venus/pkg/events"
	"github.com/filecoin-project/venus/pkg/specactors/builtin"
	"github.com/filecoin-project/venus/pkg/specactors/builtin/miner"
	"github.com/filecoin-project/venus/pkg/specactors/policy"
	"github.com/filecoin-project/venus/pkg/types"

	"github.com/filecoin-project/venus-sealer/api"
	"github.com/filecoin-project/venus-sealer/config"
	"github.com/filecoin-project/venus-sealer/constants"
	"github.com/filecoin-project/venus-sealer/journal"
	sectorstorage "github.com/filecoin-project/venus-sealer/sector-storage"
	"github.com/filecoin-project/venus-sealer/sector-storage/ffiwrapper"
	"github.com/filecoin-project/venus-sealer/service"
	sealing "github.com/filecoin-project/venus-sealer/storage-sealing"
	types2 "github.com/filecoin-project/venus-sealer/types"
)

var log = logging.Logger("storageminer")

// Miner is the central miner entrypoint object inside Lotus. It is
// instantiated in the node builder, along with the WindowPoStScheduler.
//
// This object is the owner of the sealing pipeline. Most of the actual logic
// lives in the storage-sealing module (sealing.Sealing), and the Miner object
// exposes it to the rest of the system by proxying calls.
//
// Miner#Run starts the sealing FSM.
type Miner struct {
	messager          api.IMessager
	metadataService   *service.MetadataService
	sectorInfoService *service.SectorInfoService
	logService        *service.LogService
	networkParams     *config.NetParamsConfig

	api    fullNodeFilteredAPI
	feeCfg config.MinerFeeConfig
	sealer sectorstorage.SectorManager

	sc      types2.SectorIDCounter
	verif   ffiwrapper.Verifier
	prover  ffiwrapper.Prover
	addrSel *AddressSelector

	maddr address.Address

	getSealConfig types2.GetSealingConfigFunc
	sealing       *sealing.Sealing

	sealingEvtType journal.EventType

	journal journal.Journal
}

// SealingStateEvt is a journal event that records a sector state transition.
type SealingStateEvt struct {
	SectorNumber abi.SectorNumber
	SectorType   abi.RegisteredSealProof
	From         types2.SectorState
	After        types2.SectorState
	Error        string
}

// fullNodeFilteredAPI is the subset of the full node API the Miner needs from
// a Lotus full node.
type fullNodeFilteredAPI interface {
	// Call a read only method on actors (no interaction with the chain required)
	StateCall(context.Context, *types.Message, types.TipSetKey) (*apitypes.InvocResult, error)
	StateMinerSectors(context.Context, address.Address, *bitfield.BitField, types.TipSetKey) ([]*miner.SectorOnChainInfo, error)
	StateSectorPreCommitInfo(context.Context, address.Address, abi.SectorNumber, types.TipSetKey) (miner.SectorPreCommitOnChainInfo, error)
	StateSectorGetInfo(context.Context, address.Address, abi.SectorNumber, types.TipSetKey) (*miner.SectorOnChainInfo, error)
	StateSectorPartition(ctx context.Context, maddr address.Address, sectorNumber abi.SectorNumber, tok types.TipSetKey) (*miner.SectorLocation, error)
	StateMinerInfo(context.Context, address.Address, types.TipSetKey) (miner.MinerInfo, error)
	StateMinerDeadlines(context.Context, address.Address, types.TipSetKey) ([]apitypes.Deadline, error)
	StateMinerPartitions(context.Context, address.Address, uint64, types.TipSetKey) ([]apitypes.Partition, error)
	StateMinerProvingDeadline(context.Context, address.Address, types.TipSetKey) (*dline.Info, error)
	StateMinerPreCommitDepositForPower(context.Context, address.Address, miner.SectorPreCommitInfo, types.TipSetKey) (types.BigInt, error)
	StateMinerInitialPledgeCollateral(context.Context, address.Address, miner.SectorPreCommitInfo, types.TipSetKey) (types.BigInt, error)
	StateMinerSectorAllocated(context.Context, address.Address, abi.SectorNumber, types.TipSetKey) (bool, error)
	StateSearchMsg(ctx context.Context, from types.TipSetKey, msg cid.Cid, limit abi.ChainEpoch, allowReplaced bool) (*apitypes.MsgLookup, error)
	StateWaitMsg(ctx context.Context, cid cid.Cid, confidence uint64, limit abi.ChainEpoch, allowReplaced bool) (*apitypes.MsgLookup, error)
	StateGetActor(ctx context.Context, actor address.Address, ts types.TipSetKey) (*types.Actor, error)
	StateMarketStorageDeal(context.Context, abi.DealID, types.TipSetKey) (*apitypes.MarketDeal, error)
	StateMinerFaults(context.Context, address.Address, types.TipSetKey) (bitfield.BitField, error)
	StateMinerRecoveries(context.Context, address.Address, types.TipSetKey) (bitfield.BitField, error)
	StateAccountKey(context.Context, address.Address, types.TipSetKey) (address.Address, error)
	StateNetworkVersion(context.Context, types.TipSetKey) (network.Version, error)
	StateLookupID(context.Context, address.Address, types.TipSetKey) (address.Address, error)

	StateGetReceipt(context.Context, cid.Cid, types.TipSetKey) (*types.MessageReceipt, error)

	MpoolPushMessage(context.Context, *types.Message, *types.MessageSendSpec) (*types.SignedMessage, error)

	GasEstimateMessageGas(context.Context, *types.Message, *types.MessageSendSpec, types.TipSetKey) (*types.Message, error)
	GasEstimateFeeCap(context.Context, *types.Message, int64, types.TipSetKey) (types.BigInt, error)
	GasEstimateGasPremium(_ context.Context, nblocksincl uint64, sender address.Address, gaslimit int64, tsk types.TipSetKey) (types.BigInt, error)

	ChainHead(context.Context) (*types.TipSet, error)
	ChainNotify(context.Context) (<-chan []*chain.HeadChange, error)
	ChainGetRandomnessFromTickets(ctx context.Context, tsk types.TipSetKey, personalization crypto.DomainSeparationTag, randEpoch abi.ChainEpoch, entropy []byte) (abi.Randomness, error)
	ChainGetRandomnessFromBeacon(ctx context.Context, tsk types.TipSetKey, personalization crypto.DomainSeparationTag, randEpoch abi.ChainEpoch, entropy []byte) (abi.Randomness, error)
	ChainGetTipSetByHeight(context.Context, abi.ChainEpoch, types.TipSetKey) (*types.TipSet, error)
	ChainGetBlockMessages(context.Context, cid.Cid) (*apitypes.BlockMessages, error)
	ChainGetMessage(ctx context.Context, mc cid.Cid) (*types.Message, error)
	ChainReadObj(context.Context, cid.Cid) ([]byte, error)
	ChainHasObj(context.Context, cid.Cid) (bool, error)
	ChainGetTipSet(ctx context.Context, key types.TipSetKey) (*types.TipSet, error)

	WalletSign(context.Context, address.Address, []byte) (*crypto.Signature, error)
	WalletBalance(context.Context, address.Address) (types.BigInt, error)
	WalletHas(context.Context, address.Address) (bool, error)
}

func NewMiner(api fullNodeFilteredAPI,
	messager api.IMessager,
	maddr address.Address,
	metaService *service.MetadataService,
	sectorInfoService *service.SectorInfoService,
	logService *service.LogService,
	sealer sectorstorage.SectorManager,
	sc types2.SectorIDCounter,
	verif ffiwrapper.Verifier,
	prover ffiwrapper.Prover,
	gsd types2.GetSealingConfigFunc,
	feeCfg config.MinerFeeConfig,
	journal journal.Journal,
	as *AddressSelector,
	networkParams *config.NetParamsConfig) (*Miner, error) {
	m := &Miner{
		api:               api,
		messager:          messager,
		feeCfg:            feeCfg,
		sealer:            sealer,
		metadataService:   metaService,
		sectorInfoService: sectorInfoService,
		sc:                sc,
		verif:             verif,
		prover:            prover,
		addrSel:           as,
		networkParams:     networkParams,
		maddr:             maddr,
		getSealConfig:     gsd,
		journal:           journal,
		logService:        logService,
		sealingEvtType:    journal.RegisterEventType("storage", "sealing_states"),
	}

	return m, nil
}

// Run starts the sealing FSM in the background, running preliminary checks first.
func (m *Miner) Run(ctx context.Context) error {
	if err := m.runPreflightChecks(ctx); err != nil {
		return xerrors.Errorf("miner preflight checks failed: %w", err)
	}

	md, err := m.api.StateMinerProvingDeadline(ctx, m.maddr, types.EmptyTSK)
	if err != nil {
		return xerrors.Errorf("getting miner info: %w", err)
	}

	var (
		// consumer of chain head changes.
		evts        = events.NewEvents(ctx, m.api)
		evtsAdapter = NewEventsAdapter(evts)

		// Create a shim to glue the API required by the sealing component
		// with the API that Lotus is capable of providing.
		// The shim translates between "tipset tokens" and tipset keys, and
		// provides extra methods.
		adaptedAPI = NewSealingAPIAdapter(m.api, m.messager)

		// Instantiate a precommit policy.
		defaultDuration = policy.GetMaxSectorExpirationExtension() - (md.WPoStProvingPeriod * 2)
		provingBoundary = md.PeriodStart % md.WPoStProvingPeriod

		// TODO: Maybe we update this policy after actor upgrades?
		pcp = sealing.NewBasicPreCommitPolicy(adaptedAPI, defaultDuration, provingBoundary)

		// address selector.
		as = func(ctx context.Context, mi miner.MinerInfo, use api.AddrUse, goodFunds, minFunds abi.TokenAmount) (address.Address, abi.TokenAmount, error) {
			return m.addrSel.AddressFor(ctx, m.api, m.messager, mi, use, goodFunds, minFunds)
		}

		// sealing configuration.
		cfg = types2.GetSealingConfigFunc(m.getSealConfig)
	)

	// Instantiate the sealing FSM.
	m.sealing = sealing.New(ctx, adaptedAPI, m.feeCfg, evtsAdapter, m.maddr, m.metadataService, m.sectorInfoService, m.logService, m.sealer, m.sc, m.verif, m.prover,
		&pcp, cfg, m.handleSealingNotifications, as, m.networkParams)

	// Run the sealing FSM.
	go m.sealing.Run(ctx) //nolint:errcheck // logged intside the function

	return nil
}

func (m *Miner) handleSealingNotifications(before, after types2.SectorInfo) {
	m.journal.RecordEvent(m.sealingEvtType, func() interface{} {
		return SealingStateEvt{
			SectorNumber: before.SectorNumber,
			SectorType:   before.SectorType,
			From:         before.State,
			After:        after.State,
			Error:        after.LastErr,
		}
	})
}

func (m *Miner) Stop(ctx context.Context) error {
	return m.sealing.Stop(ctx)
}

// runPreflightChecks verifies that preconditions to run the miner are satisfied.
func (m *Miner) runPreflightChecks(ctx context.Context) error {
	mi, err := m.api.StateMinerInfo(ctx, m.maddr, types.EmptyTSK)
	if err != nil {
		return xerrors.Errorf("failed to resolve miner info: %w", err)
	}

	workerKey, err := m.api.StateAccountKey(ctx, mi.Worker, types.EmptyTSK)
	if err != nil {
		return xerrors.Errorf("failed to resolve worker key: %w", err)
	}

	//todo :: check from wallet
	has, err := m.messager.WalletHas(ctx, workerKey)
	if err != nil {
		return xerrors.Errorf("failed to check wallet for worker key: %w", err)
	}

	if !has {
		return errors.New("key for worker not found in local wallet")
	}

	log.Infof("starting up miner %s, worker addr %s", m.maddr, workerKey)
	return nil
}

type StorageWpp struct {
	prover   storage.Prover
	verifier ffiwrapper.Verifier
	miner    abi.ActorID
	winnRpt  abi.RegisteredPoStProof
}

func NewWinningPoStProver(api api.FullNode, prover storage.Prover, verifier ffiwrapper.Verifier, miner types2.MinerID) (*StorageWpp, error) {
	ma, err := address.NewIDAddress(uint64(miner))
	if err != nil {
		return nil, err
	}

	mi, err := api.StateMinerInfo(context.TODO(), ma, types.EmptyTSK)
	if err != nil {
		return nil, xerrors.Errorf("getting sector size: %w", err)
	}

	if constants.InsecurePoStValidation {
		log.Warn("*****************************************************************************")
		log.Warn(" Generating fake PoSt proof! You should only see this while running tests! ")
		log.Warn("*****************************************************************************")
	}

	return &StorageWpp{prover, verifier, abi.ActorID(miner), mi.WindowPoStProofType}, nil
}

type WinningPoStProver interface {
	GenerateCandidates(context.Context, abi.PoStRandomness, uint64) ([]uint64, error)
	ComputeProof(context.Context, []proof2.SectorInfo, abi.PoStRandomness) ([]proof2.PoStProof, error)
}

var _ WinningPoStProver = (*StorageWpp)(nil)

func (wpp *StorageWpp) GenerateCandidates(ctx context.Context, randomness abi.PoStRandomness, eligibleSectorCount uint64) ([]uint64, error) {
	start := constants.Clock.Now()

	cds, err := wpp.verifier.GenerateWinningPoStSectorChallenge(ctx, wpp.winnRpt, wpp.miner, randomness, eligibleSectorCount)
	if err != nil {
		return nil, xerrors.Errorf("failed to generate candidates: %w", err)
	}
	log.Infof("Generate candidates took %s (C: %+v)", time.Since(start), cds)
	return cds, nil
}

func (wpp *StorageWpp) ComputeProof(ctx context.Context, ssi []builtin.SectorInfo, rand abi.PoStRandomness) ([]builtin.PoStProof, error) {
	if constants.InsecurePoStValidation {
		return []builtin.PoStProof{{ProofBytes: []byte("valid proof")}}, nil
	}

	log.Infof("Computing WinningPoSt ;%+v; %v", ssi, rand)

	start := constants.Clock.Now()
	proof, err := wpp.prover.GenerateWinningPoSt(ctx, wpp.miner, ssi, rand)
	if err != nil {
		return nil, err
	}
	log.Infof("GenerateWinningPoSt took %s", time.Since(start))
	return proof, nil
}
