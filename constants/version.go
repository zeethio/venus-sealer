package constants

import (
	"fmt"
	"os"
)

var CurrentCommit string

//nolint:varcheck,deadcode
const (
	majorMask = 0xff0000
	minorMask = 0xffff00
	patchMask = 0xffffff

	majorOnlyMask = 0xff0000
	minorOnlyMask = 0x00ff00
	patchOnlyMask = 0x0000ff
)

type Version uint32

func newVer(major, minor, patch uint8) Version {
	return Version(uint32(major)<<16 | uint32(minor)<<8 | uint32(patch))
}

// Ints returns (major, minor, patch) versions
func (ve Version) Ints() (uint32, uint32, uint32) {
	v := uint32(ve)
	return (v & majorOnlyMask) >> 16, (v & minorOnlyMask) >> 8, v & patchOnlyMask
}

func (ve Version) String() string {
	vmj, vmi, vp := ve.Ints()
	return fmt.Sprintf("%d.%d.%d", vmj, vmi, vp)
}

func (ve Version) EqMajorMinor(v2 Version) bool {
	return ve&minorMask == v2&minorMask
}

// BuildVersion is the local build version, set by build system
const BuildVersion = "1.2.6"

func UserVersion() string {
	if os.Getenv("VENUS_SEALER_VERSION_IGNORE_COMMIT") == "1" {
		return BuildVersion
	}

	return BuildVersion + CurrentCommit
}
