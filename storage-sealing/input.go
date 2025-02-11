package sealing

import (
	"context"
	"sort"
	"time"

	"github.com/ipfs/go-cid"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-padreader"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-statemachine"
	"github.com/filecoin-project/specs-storage/storage"

	sectorstorage "github.com/filecoin-project/venus-sealer/sector-storage"
	"github.com/filecoin-project/venus-sealer/sector-storage/ffiwrapper"
	"github.com/filecoin-project/venus-sealer/storage-sealing/sealiface"
	"github.com/filecoin-project/venus-sealer/types"
)

func (m *Sealing) handleWaitDeals(ctx statemachine.Context, sector types.SectorInfo) error {
	var used abi.UnpaddedPieceSize
	for _, piece := range sector.Pieces {
		used += piece.Piece.Size.Unpadded()
	}

	m.inputLk.Lock()

	if m.creating != nil && *m.creating == sector.SectorNumber {
		m.creating = nil
	}

	sid := m.minerSectorID(sector.SectorNumber)

	if len(m.assignedPieces[sid]) > 0 {
		m.inputLk.Unlock()
		// got assigned more pieces in the AddPiece state
		return ctx.Send(SectorAddPiece{})
	}

	started, err := m.maybeStartSealing(ctx, sector, used)
	if err != nil || started {
		delete(m.openSectors, m.minerSectorID(sector.SectorNumber))

		m.inputLk.Unlock()

		return err
	}

	if _, has := m.openSectors[sid]; !has {
		m.openSectors[sid] = &openSector{
			used: used,
			maybeAccept: func(cid cid.Cid) error {
				// todo check deal start deadline (configurable)
				m.assignedPieces[sid] = append(m.assignedPieces[sid], cid)

				return ctx.Send(SectorAddPiece{})
			},
		}
	}

	go func() {
		defer m.inputLk.Unlock()
		if err := m.updateInput(ctx.Context(), sector.SectorType); err != nil {
			log.Errorf("%+v", err)
		}
	}()

	return nil
}

func (m *Sealing) maybeStartSealing(ctx statemachine.Context, sector types.SectorInfo, used abi.UnpaddedPieceSize) (bool, error) {
	now := time.Now()
	st := m.sectorTimers[m.minerSectorID(sector.SectorNumber)]
	if st != nil {
		if !st.Stop() { // timer expired, SectorStartPacking was/is being sent
			// we send another SectorStartPacking in case one was sent in the handleAddPiece state
			log.Infow("starting to seal deal sector", "sector", sector.SectorNumber, "trigger", "wait-timeout")
			return true, ctx.Send(SectorStartPacking{})
		}
	}

	ssize, err := sector.SectorType.SectorSize()
	if err != nil {
		return false, xerrors.Errorf("getting sector size")
	}

	maxDeals, err := getDealPerSectorLimit(ssize)
	if err != nil {
		return false, xerrors.Errorf("getting per-sector deal limit: %w", err)
	}

	if len(sector.DealIDs()) >= maxDeals {
		// can't accept more deals
		log.Infow("starting to seal deal sector", "sector", sector.SectorNumber, "trigger", "maxdeals")
		return true, ctx.Send(SectorStartPacking{})
	}

	if used.Padded() == abi.PaddedPieceSize(ssize) {
		// sector full
		log.Infow("starting to seal deal sector", "sector", sector.SectorNumber, "trigger", "filled")
		return true, ctx.Send(SectorStartPacking{})
	}

	if sector.CreationTime != 0 {
		cfg, err := m.getConfig()
		if err != nil {
			return false, xerrors.Errorf("getting storage config: %w", err)
		}

		// todo check deal age, start sealing if any deal has less than X (configurable) to start deadline
		sealTime := time.Unix(sector.CreationTime, 0).Add(cfg.WaitDealsDelay)

		if now.After(sealTime) {
			log.Infow("starting to seal deal sector", "sector", sector.SectorNumber, "trigger", "wait-timeout")
			return true, ctx.Send(SectorStartPacking{})
		}

		m.sectorTimers[m.minerSectorID(sector.SectorNumber)] = time.AfterFunc(sealTime.Sub(now), func() {
			log.Infow("starting to seal deal sector", "sector", sector.SectorNumber, "trigger", "wait-timer")

			if err := ctx.Send(SectorStartPacking{}); err != nil {
				log.Errorw("sending SectorStartPacking event failed", "sector", sector.SectorNumber, "error", err)
			}
		})
	}

	return false, nil
}

func (m *Sealing) handleAddPiece(ctx statemachine.Context, sector types.SectorInfo) error {
	ssize, err := sector.SectorType.SectorSize()
	if err != nil {
		return err
	}

	res := SectorPieceAdded{}

	m.inputLk.Lock()

	pending, ok := m.assignedPieces[m.minerSectorID(sector.SectorNumber)]
	if ok {
		delete(m.assignedPieces, m.minerSectorID(sector.SectorNumber))
	}
	m.inputLk.Unlock()
	if !ok {
		// nothing to do here (might happen after a restart in AddPiece)
		return ctx.Send(res)
	}

	var offset abi.UnpaddedPieceSize
	pieceSizes := make([]abi.UnpaddedPieceSize, len(sector.Pieces))
	for i, p := range sector.Pieces {
		pieceSizes[i] = p.Piece.Size.Unpadded()
		offset += p.Piece.Size.Unpadded()
	}

	maxDeals, err := getDealPerSectorLimit(ssize)
	if err != nil {
		return xerrors.Errorf("getting per-sector deal limit: %w", err)
	}

	for i, piece := range pending {
		m.inputLk.Lock()
		deal, ok := m.pendingPieces[piece]
		m.inputLk.Unlock()
		if !ok {
			return xerrors.Errorf("piece %s assigned to sector %d not found", piece, sector.SectorNumber)
		}

		if len(sector.DealIDs())+(i+1) > maxDeals {
			// todo: this is rather unlikely to happen, but in case it does, return the deal to waiting queue instead of failing it
			deal.accepted(sector.SectorNumber, offset, xerrors.Errorf("too many deals assigned to sector %d, dropping deal", sector.SectorNumber))
			continue
		}

		pads, padLength := ffiwrapper.GetRequiredPadding(offset.Padded(), deal.size.Padded())

		if offset.Padded()+padLength+deal.size.Padded() > abi.PaddedPieceSize(ssize) {
			// todo: this is rather unlikely to happen, but in case it does, return the deal to waiting queue instead of failing it
			deal.accepted(sector.SectorNumber, offset, xerrors.Errorf("piece %s assigned to sector %d with not enough space", piece, sector.SectorNumber))
			continue
		}

		offset += padLength.Unpadded()

		for _, p := range pads {
			ppi, err := m.sealer.AddPiece(sectorstorage.WithPriority(ctx.Context(), types.DealSectorPriority),
				m.minerSector(sector.SectorType, sector.SectorNumber),
				pieceSizes,
				p.Unpadded(),
				NewNullReader(p.Unpadded()))
			if err != nil {
				err = xerrors.Errorf("writing padding piece: %w", err)
				deal.accepted(sector.SectorNumber, offset, err)
				return ctx.Send(SectorAddPieceFailed{err})
			}

			pieceSizes = append(pieceSizes, p.Unpadded())
			res.NewPieces = append(res.NewPieces, types.Piece{
				Piece: ppi,
			})
		}

		ppi, err := m.sealer.AddPiece(sectorstorage.WithPriority(ctx.Context(), types.DealSectorPriority),
			m.minerSector(sector.SectorType, sector.SectorNumber),
			pieceSizes,
			deal.size,
			deal.data)
		if err != nil {
			err = xerrors.Errorf("writing piece: %w", err)
			deal.accepted(sector.SectorNumber, offset, err)
			return ctx.Send(SectorAddPieceFailed{err})
		}

		log.Infow("deal added to a sector", "deal", deal.deal.DealID, "sector", sector.SectorNumber, "piece", ppi.PieceCID)

		deal.accepted(sector.SectorNumber, offset, nil)

		offset += deal.size
		pieceSizes = append(pieceSizes, deal.size)

		res.NewPieces = append(res.NewPieces, types.Piece{
			Piece:    ppi,
			DealInfo: &deal.deal,
		})
	}

	return ctx.Send(res)
}

// nolint
func (m *Sealing) handleAddPieceFailed(ctx statemachine.Context, sector types.SectorInfo) error {
	log.Errorf("No recovery plan for AddPiece failing")
	// todo: cleanup sector / just go retry (requires adding offset param to AddPiece in sector-storage for this to be safe)
	return nil
}

func (m *Sealing) AddPieceToAnySector(ctx context.Context, size abi.UnpaddedPieceSize, data storage.Data, deal types.DealInfo) (abi.SectorNumber, abi.PaddedPieceSize, error) {
	log.Infof("Adding piece for deal %d (publish msg: %s)", deal.DealID, deal.PublishCid)
	if (padreader.PaddedSize(uint64(size))) != size {
		return 0, 0, xerrors.Errorf("cannot allocate unpadded piece")
	}

	sp, err := m.currentSealProof(ctx)
	if err != nil {
		return 0, 0, xerrors.Errorf("getting current seal proof type: %w", err)
	}

	ssize, err := sp.SectorSize()
	if err != nil {
		return 0, 0, err
	}

	if size > abi.PaddedPieceSize(ssize).Unpadded() {
		return 0, 0, xerrors.Errorf("piece cannot fit into a sector")
	}

	if _, err := deal.DealProposal.Cid(); err != nil {
		return 0, 0, xerrors.Errorf("getting proposal CID: %w", err)
	}

	m.inputLk.Lock()
	if _, exist := m.pendingPieces[proposalCID(deal)]; exist {
		m.inputLk.Unlock()
		return 0, 0, xerrors.Errorf("piece for deal %s already pending", proposalCID(deal))
	}

	resCh := make(chan struct {
		sn     abi.SectorNumber
		offset abi.UnpaddedPieceSize
		err    error
	}, 1)

	m.pendingPieces[proposalCID(deal)] = &pendingPiece{
		size:     size,
		deal:     deal,
		data:     data,
		assigned: false,
		accepted: func(sn abi.SectorNumber, offset abi.UnpaddedPieceSize, err error) {
			resCh <- struct {
				sn     abi.SectorNumber
				offset abi.UnpaddedPieceSize
				err    error
			}{sn: sn, offset: offset, err: err}
		},
	}

	go func() {
		defer m.inputLk.Unlock()
		if err := m.updateInput(ctx, sp); err != nil {
			log.Errorf("%+v", err)
		}
	}()

	res := <-resCh

	return res.sn, res.offset.Padded(), res.err
}

// called with m.inputLk
func (m *Sealing) updateInput(ctx context.Context, sp abi.RegisteredSealProof) error {
	ssize, err := sp.SectorSize()
	if err != nil {
		return err
	}

	type match struct {
		sector abi.SectorID
		deal   cid.Cid

		size    abi.UnpaddedPieceSize
		padding abi.UnpaddedPieceSize
	}

	var matches []match
	toAssign := map[cid.Cid]struct{}{} // used to maybe create new sectors

	// todo: this is distinctly O(n^2), may need to be optimized for tiny deals and large scale miners
	//  (unlikely to be a problem now)
	for proposalCid, piece := range m.pendingPieces {
		if piece.assigned {
			continue // already assigned to a sector, skip
		}

		toAssign[proposalCid] = struct{}{}

		for id, sector := range m.openSectors {
			avail := abi.PaddedPieceSize(ssize).Unpadded() - sector.used

			if piece.size <= avail { // (note: if we have enough space for the piece, we also have enough space for inter-piece padding)
				matches = append(matches, match{
					sector: id,
					deal:   proposalCid,

					size:    piece.size,
					padding: avail % piece.size,
				})
			}
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].padding != matches[j].padding { // less padding is better
			return matches[i].padding < matches[j].padding
		}

		if matches[i].size != matches[j].size { // larger pieces are better
			return matches[i].size < matches[j].size
		}

		return matches[i].sector.Number < matches[j].sector.Number // prefer older sectors
	})

	var assigned int
	for _, mt := range matches {
		if m.pendingPieces[mt.deal].assigned {
			assigned++
			continue
		}

		if _, found := m.openSectors[mt.sector]; !found {
			continue
		}

		avail := abi.PaddedPieceSize(ssize).Unpadded() - m.openSectors[mt.sector].used

		if mt.size > avail {
			continue
		}

		err := m.openSectors[mt.sector].maybeAccept(mt.deal)
		if err != nil {
			m.pendingPieces[mt.deal].accepted(mt.sector.Number, 0, err) // non-error case in handleAddPiece
		}

		m.openSectors[mt.sector].used += mt.padding + mt.size

		m.pendingPieces[mt.deal].assigned = true
		delete(toAssign, mt.deal)

		if err != nil {
			log.Errorf("sector %d rejected deal %s: %+v", mt.sector, mt.deal, err)
			continue
		}
	}

	if len(toAssign) > 0 {
		if err := m.tryCreateDealSector(ctx, sp); err != nil {
			log.Errorw("Failed to create a new sector for deals", "error", err)
		}
	}

	return nil
}

func (m *Sealing) tryCreateDealSector(ctx context.Context, sp abi.RegisteredSealProof) error {
	m.startupWait.Wait()

	if m.creating != nil {
		return nil // new sector is being created right now
	}

	cfg, err := m.getConfig()
	if err != nil {
		return xerrors.Errorf("getting storage config: %w", err)
	}

	if cfg.MaxSealingSectorsForDeals > 0 && m.stats.CurSealing() >= cfg.MaxSealingSectorsForDeals {
		return nil
	}

	if cfg.MaxWaitDealsSectors > 0 && m.stats.CurStaging() >= cfg.MaxWaitDealsSectors {
		return nil
	}

	sid, err := m.createSector(ctx, cfg, sp)
	if err != nil {
		return err
	}

	m.creating = &sid

	log.Infow("Creating sector", "number", sid, "type", "deal", "proofType", sp)
	return m.sectors.Send(uint64(sid), SectorStart{
		ID:         sid,
		SectorType: sp,
	})
}

// call with m.inputLk
func (m *Sealing) createSector(ctx context.Context, cfg sealiface.Config, sp abi.RegisteredSealProof) (abi.SectorNumber, error) {
	// Now actually create a new sector

	sid, err := m.sc.Next()
	if err != nil {
		return 0, xerrors.Errorf("getting sector number: %w", err)
	}

	err = m.sealer.NewSector(ctx, m.minerSector(sp, sid))
	if err != nil {
		return 0, xerrors.Errorf("initializing sector: %w", err)
	}

	// update stats early, fsm planner would do that async
	m.stats.UpdateSector(cfg, m.minerSectorID(sid), types.UndefinedSectorState)

	return sid, nil
}

func (m *Sealing) StartPacking(sid abi.SectorNumber) error {
	m.startupWait.Wait()

	log.Infow("starting to seal deal sector", "sector", sid, "trigger", "user")
	return m.sectors.Send(uint64(sid), SectorStartPacking{})
}

func proposalCID(deal types.DealInfo) cid.Cid {
	pc, err := deal.DealProposal.Cid()
	if err != nil {
		log.Errorf("DealProposal.Cid error: %+v", err)
		return cid.Undef
	}

	return pc
}
