package libcontainer

import (
	"github.com/szcdx/runc/libcontainer/cgroups"
	"github.com/szcdx/runc/libcontainer/intelrdt"
	"github.com/szcdx/runc/types"
)

type Stats struct {
	Interfaces    []*types.NetworkInterface
	CgroupStats   *cgroups.Stats
	IntelRdtStats *intelrdt.Stats
}
