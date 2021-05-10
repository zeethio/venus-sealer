package constants

import (
	"github.com/filecoin-project/go-state-types/network"
	"github.com/filecoin-project/venus/pkg/specactors/policy"
	"github.com/raulk/clock"
)

const (
	NewestNetworkVersion = network.Version9
	MessageConfidence    = uint64(5)
	BlocksPerEpoch       = uint64(5)
)

var (
	FullAPIVersion   = newVer(1, 3, 0)
	MinerAPIVersion  = newVer(1, 0, 1)
	WorkerAPIVersion = newVer(1, 0, 0)

	MinerVersion = newVer(1, 0, 2)
)
var Clock = clock.New()
