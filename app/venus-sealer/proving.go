package main

import (
	"fmt"
	"github.com/filecoin-project/venus-sealer/api"
	"github.com/filecoin-project/venus/pkg/chain"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/fatih/color"
	"github.com/urfave/cli/v2"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/specs-storage/storage"
	"github.com/filecoin-project/venus/pkg/specactors/builtin/miner"
	"github.com/filecoin-project/venus/pkg/types"
)

var provingCmd = &cli.Command{
	Name:  "proving",
	Usage: "View proving information",
	Subcommands: []*cli.Command{
		provingInfoCmd,
		provingDeadlinesCmd,
		provingDeadlineInfoCmd,
		provingFaultsCmd,
		provingCheckProvableCmd,
	},
}

var provingFaultsCmd = &cli.Command{
	Name:  "faults",
	Usage: "View the currently known proving faulty sectors information",
	Action: func(cctx *cli.Context) error {
		color.NoColor = !cctx.Bool("color")

		storageAPI, closer, err := api.GetStorageMinerAPI(cctx)
		if err != nil {
			return err
		}
		defer closer()

		nodeAPI, acloser, err := api.GetFullNodeAPIV2(cctx)
		if err != nil {
			return err
		}
		defer acloser()

		ctx := api.ReqContext(cctx)

		stor := chain.ActorStore(ctx, api.NewAPIBlockstore(nodeAPI))

		maddr, err := getActorAddress(ctx, storageAPI, cctx.String("actor"))
		if err != nil {
			return err
		}

		mact, err := nodeAPI.StateGetActor(ctx, maddr, types.EmptyTSK)
		if err != nil {
			return err
		}

		mas, err := miner.Load(stor, mact)
		if err != nil {
			return err
		}

		fmt.Printf("Sealer: %s\n", color.BlueString("%s", maddr))

		tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "deadline\tpartition\tsectors")
		err = mas.ForEachDeadline(func(dlIdx uint64, dl miner.Deadline) error {
			return dl.ForEachPartition(func(partIdx uint64, part miner.Partition) error {
				faults, err := part.FaultySectors()
				if err != nil {
					return err
				}
				return faults.ForEach(func(num uint64) error {
					_, _ = fmt.Fprintf(tw, "%d\t%d\t%d\n", dlIdx, partIdx, num)
					return nil
				})
			})
		})
		if err != nil {
			return err
		}
		return tw.Flush()
	},
}

var provingInfoCmd = &cli.Command{
	Name:  "info",
	Usage: "View current state information",
	Action: func(cctx *cli.Context) error {
		color.NoColor = !cctx.Bool("color")

		storageAPI, closer, err := api.GetStorageMinerAPI(cctx)
		if err != nil {
			return err
		}
		defer closer()

		nodeAPI, acloser, err := api.GetFullNodeAPIV2(cctx)
		if err != nil {
			return err
		}
		defer acloser()

		ctx := api.ReqContext(cctx)

		networkParams, err := storageAPI.NetParamsConfig(ctx)
		if err != nil {
			return err
		}
		maddr, err := getActorAddress(ctx, storageAPI, cctx.String("actor"))
		if err != nil {
			return err
		}

		head, err := nodeAPI.ChainHead(ctx)
		if err != nil {
			return xerrors.Errorf("getting chain head: %w", err)
		}

		mact, err := nodeAPI.StateGetActor(ctx, maddr, head.Key())
		if err != nil {
			return err
		}

		stor := chain.ActorStore(ctx, api.NewAPIBlockstore(nodeAPI))

		mas, err := miner.Load(stor, mact)
		if err != nil {
			return err
		}

		cd, err := nodeAPI.StateMinerProvingDeadline(ctx, maddr, head.Key())
		if err != nil {
			return xerrors.Errorf("getting miner info: %w", err)
		}

		fmt.Printf("Sealer: %s\n", color.BlueString("%s", maddr))

		proving := uint64(0)
		faults := uint64(0)
		recovering := uint64(0)
		curDeadlineSectors := uint64(0)

		if err := mas.ForEachDeadline(func(dlIdx uint64, dl miner.Deadline) error {
			return dl.ForEachPartition(func(partIdx uint64, part miner.Partition) error {
				if bf, err := part.LiveSectors(); err != nil {
					return err
				} else if count, err := bf.Count(); err != nil {
					return err
				} else {
					proving += count
					if dlIdx == cd.Index {
						curDeadlineSectors += count
					}
				}

				if bf, err := part.FaultySectors(); err != nil {
					return err
				} else if count, err := bf.Count(); err != nil {
					return err
				} else {
					faults += count
				}

				if bf, err := part.RecoveringSectors(); err != nil {
					return err
				} else if count, err := bf.Count(); err != nil {
					return err
				} else {
					recovering += count
				}

				return nil
			})
		}); err != nil {
			return xerrors.Errorf("walking miner deadlines and partitions: %w", err)
		}

		var faultPerc float64
		if proving > 0 {
			faultPerc = float64(faults * 100 / proving)
		}

		fmt.Printf("Current Epoch:           %d\n", cd.CurrentEpoch)

		fmt.Printf("Proving Period Boundary: %d\n", cd.PeriodStart%cd.WPoStProvingPeriod)
		fmt.Printf("Proving Period Start:    %s\n", EpochTime(cd.CurrentEpoch, cd.PeriodStart, networkParams.BlockDelaySecs))
		fmt.Printf("Next Period Start:       %s\n\n", EpochTime(cd.CurrentEpoch, cd.PeriodStart+cd.WPoStProvingPeriod, networkParams.BlockDelaySecs))

		fmt.Printf("Faults:      %d (%.2f%%)\n", faults, faultPerc)
		fmt.Printf("Recovering:  %d\n", recovering)

		fmt.Printf("Deadline Index:       %d\n", cd.Index)
		fmt.Printf("Deadline Sectors:     %d\n", curDeadlineSectors)
		fmt.Printf("Deadline Open:        %s\n", EpochTime(cd.CurrentEpoch, cd.Open, networkParams.BlockDelaySecs))
		fmt.Printf("Deadline Close:       %s\n", EpochTime(cd.CurrentEpoch, cd.Close, networkParams.BlockDelaySecs))
		fmt.Printf("Deadline Challenge:   %s\n", EpochTime(cd.CurrentEpoch, cd.Challenge, networkParams.BlockDelaySecs))
		fmt.Printf("Deadline FaultCutoff: %s\n", EpochTime(cd.CurrentEpoch, cd.FaultCutoff, networkParams.BlockDelaySecs))
		return nil
	},
}

var provingDeadlinesCmd = &cli.Command{
	Name:  "deadlines",
	Usage: "View the current proving period deadlines information",
	Action: func(cctx *cli.Context) error {
		color.NoColor = !cctx.Bool("color")

		storageAPI, closer, err := api.GetStorageMinerAPI(cctx)
		if err != nil {
			return err
		}
		defer closer()

		nodeAPI, acloser, err := api.GetFullNodeAPIV2(cctx)
		if err != nil {
			return err
		}
		defer acloser()

		ctx := api.ReqContext(cctx)

		maddr, err := getActorAddress(ctx, storageAPI, cctx.String("actor"))
		if err != nil {
			return err
		}

		deadlines, err := nodeAPI.StateMinerDeadlines(ctx, maddr, types.EmptyTSK)
		if err != nil {
			return xerrors.Errorf("getting deadlines: %w", err)
		}

		di, err := nodeAPI.StateMinerProvingDeadline(ctx, maddr, types.EmptyTSK)
		if err != nil {
			return xerrors.Errorf("getting deadlines: %w", err)
		}

		fmt.Printf("Sealer: %s\n", color.BlueString("%s", maddr))

		tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "deadline\tpartitions\tsectors (faults)\tproven partitions")

		for dlIdx, deadline := range deadlines {
			partitions, err := nodeAPI.StateMinerPartitions(ctx, maddr, uint64(dlIdx), types.EmptyTSK)
			if err != nil {
				return xerrors.Errorf("getting partitions for deadline %d: %w", dlIdx, err)
			}

			provenPartitions, err := deadline.PostSubmissions.Count()
			if err != nil {
				return err
			}

			sectors := uint64(0)
			faults := uint64(0)

			for _, partition := range partitions {
				sc, err := partition.AllSectors.Count()
				if err != nil {
					return err
				}

				sectors += sc

				fc, err := partition.FaultySectors.Count()
				if err != nil {
					return err
				}

				faults += fc
			}

			var cur string
			if di.Index == uint64(dlIdx) {
				cur += "\t(current)"
			}
			_, _ = fmt.Fprintf(tw, "%d\t%d\t%d (%d)\t%d%s\n", dlIdx, len(partitions), sectors, faults, provenPartitions, cur)
		}

		return tw.Flush()
	},
}

var provingDeadlineInfoCmd = &cli.Command{
	Name:      "deadline",
	Usage:     "View the current proving period deadline information by its index ",
	ArgsUsage: "<deadlineIdx>",
	Action: func(cctx *cli.Context) error {

		if cctx.Args().Len() != 1 {
			return xerrors.Errorf("must pass deadline index")
		}

		dlIdx, err := strconv.ParseUint(cctx.Args().Get(0), 10, 64)
		if err != nil {
			return xerrors.Errorf("could not parse deadline index: %w", err)
		}

		storageAPI, closer, err := api.GetStorageMinerAPI(cctx)
		if err != nil {
			return err
		}
		defer closer()

		nodeAPI, acloser, err := api.GetFullNodeAPIV2(cctx)
		if err != nil {
			return err
		}
		defer acloser()

		ctx := api.ReqContext(cctx)

		maddr, err := getActorAddress(ctx, storageAPI, cctx.String("actor"))
		if err != nil {
			return err
		}

		deadlines, err := nodeAPI.StateMinerDeadlines(ctx, maddr, types.EmptyTSK)
		if err != nil {
			return xerrors.Errorf("getting deadlines: %w", err)
		}

		di, err := nodeAPI.StateMinerProvingDeadline(ctx, maddr, types.EmptyTSK)
		if err != nil {
			return xerrors.Errorf("getting deadlines: %w", err)
		}

		partitions, err := nodeAPI.StateMinerPartitions(ctx, maddr, dlIdx, types.EmptyTSK)
		if err != nil {
			return xerrors.Errorf("getting partitions for deadline %d: %w", dlIdx, err)
		}

		provenPartitions, err := deadlines[dlIdx].PostSubmissions.Count()
		if err != nil {
			return err
		}

		fmt.Printf("Deadline Index:           %d\n", dlIdx)
		fmt.Printf("Partitions:               %d\n", len(partitions))
		fmt.Printf("Proven Partitions:        %d\n", provenPartitions)
		fmt.Printf("Current:                  %t\n\n", di.Index == dlIdx)

		for pIdx, partition := range partitions {
			sectorCount, err := partition.AllSectors.Count()
			if err != nil {
				return err
			}

			sectorNumbers, err := partition.AllSectors.All(sectorCount)
			if err != nil {
				return err
			}

			faultsCount, err := partition.FaultySectors.Count()
			if err != nil {
				return err
			}

			fn, err := partition.FaultySectors.All(faultsCount)
			if err != nil {
				return err
			}

			fmt.Printf("Partition Index:          %d\n", pIdx)
			fmt.Printf("Sectors:                  %d\n", sectorCount)
			fmt.Printf("Sector Numbers:           %v\n", sectorNumbers)
			fmt.Printf("Faults:                   %d\n", faultsCount)
			fmt.Printf("Faulty Sectors:           %d\n", fn)
		}
		return nil
	},
}

var provingCheckProvableCmd = &cli.Command{
	Name:      "check",
	Usage:     "Check sectors provable",
	ArgsUsage: "<deadlineIdx>",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:  "only-bad",
			Usage: "print only bad sectors",
			Value: false,
		},
		&cli.BoolFlag{
			Name:  "slow",
			Usage: "run slower checks",
		},
	},
	Action: func(cctx *cli.Context) error {
		if cctx.Args().Len() != 1 {
			return xerrors.Errorf("must pass deadline index")
		}

		dlIdx, err := strconv.ParseUint(cctx.Args().Get(0), 10, 64)
		if err != nil {
			return xerrors.Errorf("could not parse deadline index: %w", err)
		}

		nodeAPI, closer, err := api.GetFullNodeAPIV2(cctx)
		if err != nil {
			return err
		}
		defer closer()

		storageAPI, scloser, err := api.GetStorageMinerAPI(cctx)
		if err != nil {
			return err
		}
		defer scloser()

		ctx := api.ReqContext(cctx)

		addr, err := storageAPI.ActorAddress(ctx)
		if err != nil {
			return err
		}

		mid, err := address.IDFromAddress(addr)
		if err != nil {
			return err
		}

		info, err := nodeAPI.StateMinerInfo(ctx, addr, types.EmptyTSK)
		if err != nil {
			return err
		}

		partitions, err := nodeAPI.StateMinerPartitions(ctx, addr, dlIdx, types.EmptyTSK)
		if err != nil {
			return err
		}

		tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "deadline\tpartition\tsector\tstatus")

		for parIdx, par := range partitions {
			sectors := make(map[abi.SectorNumber]struct{})

			sectorInfos, err := nodeAPI.StateMinerSectors(ctx, addr, &par.LiveSectors, types.EmptyTSK)
			if err != nil {
				return err
			}

			var tocheck []storage.SectorRef
			for _, info := range sectorInfos {
				sectors[info.SectorNumber] = struct{}{}
				tocheck = append(tocheck, storage.SectorRef{
					ProofType: info.SealProof,
					ID: abi.SectorID{
						Miner:  abi.ActorID(mid),
						Number: info.SectorNumber,
					},
				})
			}

			bad, err := storageAPI.CheckProvable(ctx, info.WindowPoStProofType, tocheck, cctx.Bool("slow"))
			if err != nil {
				return err
			}

			for s := range sectors {
				if err, exist := bad[s]; exist {
					_, _ = fmt.Fprintf(tw, "%d\t%d\t%d\t%s\n", dlIdx, parIdx, s, color.RedString("bad")+fmt.Sprintf(" (%s)", err))
				} else if !cctx.Bool("only-bad") {
					_, _ = fmt.Fprintf(tw, "%d\t%d\t%d\t%s\n", dlIdx, parIdx, s, color.GreenString("good"))
				}
			}
		}

		return tw.Flush()
	},
}
