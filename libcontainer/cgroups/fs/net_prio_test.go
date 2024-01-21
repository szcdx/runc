package fs

import (
	"strings"
	"testing"

	"github.com/szcdx/runc/libcontainer/cgroups/fscommon"
	"github.com/szcdx/runc/libcontainer/configs"
)

var prioMap = []*configs.IfPrioMap{
	{
		Interface: "test",
		Priority:  5,
	},
}

func TestNetPrioSetIfPrio(t *testing.T) {
	path := tempDir(t, "net_prio")

	r := &configs.Resources{
		NetPrioIfpriomap: prioMap,
	}
	netPrio := &NetPrioGroup{}
	if err := netPrio.Set(path, r); err != nil {
		t.Fatal(err)
	}

	value, err := fscommon.GetCgroupParamString(path, "net_prio.ifpriomap")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(value, "test 5") {
		t.Fatal("Got the wrong value, set net_prio.ifpriomap failed.")
	}
}
