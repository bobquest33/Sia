package contractor

import (
	"testing"
	"time"

	"github.com/NebulousLabs/Sia/build"
	"github.com/NebulousLabs/Sia/modules"
	"github.com/NebulousLabs/Sia/types"
)

// offlineHostDB overrides an existing hostDB so that it returns a modified
// scan history for a specific host.
type offlineHostDB struct {
	hostDB
	addr modules.NetAddress
}

// Host returns the host with address addr. If addr matches hdb.addr, the
// host's scan history will be modified to make the host appear offline.
func (hdb offlineHostDB) Host(addr modules.NetAddress) (modules.HostDBEntry, bool) {
	host, ok := hdb.hostDB.Host(addr)
	if ok && addr == hdb.addr {
		// fake three scans over the past uptimeWindow, all of which failed
		badScan1 := modules.HostDBScan{Timestamp: time.Now().Add(-uptimeWindow * 2), Success: false}
		badScan2 := modules.HostDBScan{Timestamp: time.Now().Add(-uptimeWindow), Success: false}
		badScan3 := modules.HostDBScan{Timestamp: time.Now(), Success: false}
		host.ScanHistory = []modules.HostDBScan{badScan1, badScan2, badScan3}
	}
	return host, ok
}

// TestIntegrationReplaceOffline tests that when a host goes offline, its
// contract is eventually replaced.
func TestIntegrationReplaceOffline(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()
	hosts, c, m, err := newTestingTrio("TestIntegrationMonitorUptime", 1)
	if err != nil {
		t.Fatal(err)
	}
	h := hosts[0]
	defer h.Close()

	// override IsOffline to always return true for h
	c.hdb = offlineHostDB{c.hdb, h.ExternalSettings().NetAddress}

	// create another host
	dir := build.TempDir("contractor", "TestIntegrationMonitorUptime", "Host2")
	h2, err := newTestingHost(dir, c.cs.(modules.ConsensusSet), c.tpool.(modules.TransactionPool))
	if err != nil {
		t.Fatal(err)
	}

	// form a contract with h
	c.SetAllowance(modules.Allowance{
		Funds:       types.SiacoinPrecision.Mul64(100),
		Hosts:       1,
		Period:      100,
		RenewWindow: 10,
	})
	// we should have a contract, but it will be marked as offline due to the
	// hocked hostDB
	if len(c.contracts) != 1 {
		t.Fatal("contract not formed")
	}
	if len(c.onlineContracts()) != 0 {
		t.Fatal("contract should not be reported as online")
	}

	// announce the second host
	err = h2.Announce()
	if err != nil {
		t.Fatal(err)
	}

	// mine a block, processing the announcement
	m.AddBlock()

	// wait for hostdb to scan host
	for i := 0; i < 100 && len(c.hdb.RandomHosts(2, nil)) != 2; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if len(c.hdb.RandomHosts(2, nil)) != 2 {
		t.Fatal("host did not make it into the contractor hostdb in time", c.hdb.RandomHosts(2, nil))
	}

	// mine a block and wait for a new contract is formed. ProcessConsensusChange will
	// trigger managedFormAllowanceContracts, which should form a new contract
	// with h2
	m.AddBlock()
	for i := 0; i < 100 && len(c.Contracts()) != 1; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if len(c.Contracts()) != 1 {
		t.Fatal("contract was not replaced")
	}
	if c.Contracts()[0].NetAddress != h2.ExternalSettings().NetAddress {
		t.Fatal("contractor formed replacement contract with wrong host")
	}
}

// mapHostDB is a hostDB that implements the Host method via a simple map.
type mapHostDB struct {
	stubHostDB
	hosts map[modules.NetAddress]modules.HostDBEntry
}

func (m mapHostDB) Host(addr modules.NetAddress) (modules.HostDBEntry, bool) {
	h, exists := m.hosts[addr]
	return h, exists
}

// TestIsOffline tests the IsOffline method.
func TestIsOffline(t *testing.T) {
	now := time.Now()
	oldBadScan := modules.HostDBScan{Timestamp: now.Add(-uptimeWindow * 2), Success: false}
	oldGoodScan := modules.HostDBScan{Timestamp: now.Add(-uptimeWindow * 2), Success: true}
	newBadScan := modules.HostDBScan{Timestamp: now.Add(-uptimeWindow / 2), Success: false}
	newGoodScan := modules.HostDBScan{Timestamp: now.Add(-uptimeWindow / 2), Success: true}
	currentBadScan := modules.HostDBScan{Timestamp: now, Success: false}
	currentGoodScan := modules.HostDBScan{Timestamp: now, Success: true}

	tests := []struct {
		scans   []modules.HostDBScan
		offline bool
	}{
		// no data
		{nil, false},
		// not enough data
		{[]modules.HostDBScan{oldBadScan, newGoodScan}, false},
		// data covers small range
		{[]modules.HostDBScan{oldBadScan, oldBadScan, oldBadScan}, false},
		// data covers large range, but at least 1 scan succeded
		{[]modules.HostDBScan{oldBadScan, newGoodScan, currentBadScan}, false},
		// data covers large range, no scans succeded
		{[]modules.HostDBScan{oldBadScan, newBadScan, currentBadScan}, true},
		// recent scan was good (and within uptimeWindow of oldBadScan, but that shouldn't matter)
		{[]modules.HostDBScan{oldBadScan, oldBadScan, oldBadScan, oldGoodScan}, false},
		// recent scan was good (and outside uptimeWindow of oldBadScan, but that shouldn't matter)
		{[]modules.HostDBScan{oldBadScan, oldBadScan, oldBadScan, currentGoodScan}, false},
	}
	for i, test := range tests {
		// construct a contractor with a hostdb containing the scans
		c := &Contractor{
			hdb: mapHostDB{
				hosts: map[modules.NetAddress]modules.HostDBEntry{
					"foo": {ScanHistory: test.scans},
				},
			},
		}
		if offline := c.IsOffline("foo"); offline != test.offline {
			t.Errorf("IsOffline(%v) = %v, expected %v", i, offline, test.offline)
		}
	}
}
