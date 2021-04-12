package main

import (
	"github.com/filecoin-project/venus-sealer/config"
	"github.com/filecoin-project/venus-sealer/models/sqlite"
	"log"
	"os"
	"runtime/pprof"
	"sync"
)

func main() {

	repo, err := sqlite.OpenSqlite(&config.SqliteConfig{Path: "./sealer.db"})
	if err != nil {
		log.Fatal(err)
		return
	}
	repo.AutoMigrate()
	/*
		cccid, _ := cid.Decode("bafy2bzacedoccbsb4sjcyuur55xj67yg2sorp5xhl366sl5acdfqg4kcq2yty")

		seq := uint64(10000)
		from := uint64(80000)
		for i:=from;i<from+seq;i++ {
			err = repo.SectorInfoRepo().Save(&types.SectorInfo{
				State:            "Pxxxxx",
				SectorNumber:     abi.SectorNumber(i),
				SectorType:       5,
				Pieces:           nil,
				TicketValue:      []byte("xxxxdsfcrtgrtehbvrxxxxx"),
				TicketEpoch:      100000,
				PreCommit1Out:     []byte("fvhcrntgiogrjgforiguhrebonwcigmrwj"),
				CommD:            &cccid,
				CommR:            &cccid,
				Proof:             []byte("sdfcrtuncgjf2oeailmwcjhfnudghrjwefrpiocghjnrehcfeurcfgjwimhiuxjeuwifhwuqifhuxfvhcrntgiogrjgforiguhrebonwcigmrwj"),
				PreCommitInfo:    &miner.SectorPreCommitInfo{
					SealProof:              10,
					SectorNumber:           100,
					SealedCID:              cccid,
					SealRandEpoch:          10000,
					DealIDs:                []abi.DealID{1,2,3,45,1,23},
					Expiration:             100000000,
					ReplaceCapacity:        false,
					ReplaceSectorDeadline:  0,
					ReplaceSectorPartition: 0,
					ReplaceSectorNumber:    0,
				},
				PreCommitDeposit:  big.NewInt(10000),
				PreCommitMessage: cccid.String(),
				PreCommitTipSet:  nil,
				PreCommit2Fails:  0,
				SeedValue:        []byte("xsdhfxwgveirujfnwejklfcmnqejklrbvwfxgrbcfexrewfcewxqewzdwegrxrth5yerthcrthge"),
				SeedEpoch:        1000,
				CommitMessage:    cccid.String(),
				InvalidProofs:    0,
				FaultReportMsg:  cccid.String(),
				Return:           "svdxetcrexfdeawqdzewfxresxdqe",
				TerminateMessage: cccid.String(),
				TerminatedAt:     0,
				LastErr:          "svdxetcrexfdeawqdzewfxresxdqe",
			})
			if err != nil {
				log.Fatal(err)
				return
			}
		}*/

	f, _ := os.Create("./cpu.pprof")
	pprof.StartCPUProfile(f)
	defer pprof.StopCPUProfile()
	var wg sync.WaitGroup
	for i := 10; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			repo.SectorInfoRepo().GetSectorInfoByID(uint64(i))
		}()
	}

	wg.Wait()
}
