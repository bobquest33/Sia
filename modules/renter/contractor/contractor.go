package contractor

import (
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/NebulousLabs/Sia/crypto"
	"github.com/NebulousLabs/Sia/modules"
	"github.com/NebulousLabs/Sia/persist"
	siasync "github.com/NebulousLabs/Sia/sync"
	"github.com/NebulousLabs/Sia/types"
)

var (
	errNilCS     = errors.New("cannot create contractor with nil consensus set")
	errNilWallet = errors.New("cannot create contractor with nil wallet")
	errNilTpool  = errors.New("cannot create contractor with nil transaction pool")

	// COMPATv1.0.4-lts
	// metricsContractID identifies a special contract that contains aggregate
	// financial metrics from older contractors
	metricsContractID = types.FileContractID{'m', 'e', 't', 'r', 'i', 'c', 's'}
)

// A cachedRevision contains changes that would be applied to a RenterContract
// if a contract revision succeeded. The contractor must cache these changes
// as a safeguard against desynchronizing with the host.
// TODO: save a diff of the Merkle roots instead of all of them.
type cachedRevision struct {
	Revision    types.FileContractRevision
	MerkleRoots []crypto.Hash
}

// A Contractor negotiates, revises, renews, and provides access to file
// contracts.
type Contractor struct {
	// dependencies
	cs      consensusSet
	hdb     hostDB
	log     *persist.Logger
	persist persister
	tpool   transactionPool
	wallet  wallet

	allowance       modules.Allowance
	blockHeight     types.BlockHeight
	cachedRevisions map[types.FileContractID]cachedRevision
	contracts       map[types.FileContractID]modules.RenterContract
	currentPeriod   types.BlockHeight
	downloaders     map[types.FileContractID]*hostDownloader
	editors         map[types.FileContractID]*hostEditor
	lastChange      modules.ConsensusChangeID
	oldContracts    map[types.FileContractID]modules.RenterContract
	renewedIDs      map[types.FileContractID]types.FileContractID
	renewing        map[types.FileContractID]bool // prevent revising during renewal
	revising        map[types.FileContractID]bool // prevent overlapping revisions

	mu sync.RWMutex

	// in addition to mu, a separate lock enforces that multiple goroutines
	// won't try to simultaneously edit the contract set.
	editLock siasync.TryMutex
}

// Allowance returns the current allowance.
func (c *Contractor) Allowance() modules.Allowance {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.allowance
}

// Contract returns the latest contract formed with the specified host.
func (c *Contractor) Contract(hostAddr modules.NetAddress) (modules.RenterContract, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, c := range c.contracts {
		if c.NetAddress == hostAddr {
			return c, true
		}
	}
	return modules.RenterContract{}, false
}

// Contracts returns the contracts formed by the contractor in the current
// allowance period. Only contracts formed with currently online hosts are
// returned.
func (c *Contractor) Contracts() (cs []modules.RenterContract) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.onlineContracts()
}

// AllContracts returns the contracts formed by the contractor in the current
// allowance period.
func (c *Contractor) AllContracts() (cs []modules.RenterContract) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, contract := range c.contracts {
		cs = append(cs, contract)
	}
	// COMPATv1.0.4-lts
	// also return the special metrics contract (see persist.go)
	for _, contract := range c.oldContracts {
		if contract.ID == metricsContractID {
			cs = append(cs, contract)
		}
	}
	return
}

// CurrentPeriod returns the height at which the current allowance period
// began.
func (c *Contractor) CurrentPeriod() types.BlockHeight {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.currentPeriod
}

// resolveID returns the ID of the most recent renewal of id.
func (c *Contractor) resolveID(id types.FileContractID) types.FileContractID {
	if newID, ok := c.renewedIDs[id]; ok && newID != id {
		return c.resolveID(newID)
	}
	return id
}

// New returns a new Contractor.
func New(cs consensusSet, wallet walletShim, tpool transactionPool, hdb hostDB, persistDir string) (*Contractor, error) {
	// Check for nil inputs.
	if cs == nil {
		return nil, errNilCS
	}
	if wallet == nil {
		return nil, errNilWallet
	}
	if tpool == nil {
		return nil, errNilTpool
	}

	// Create the persist directory if it does not yet exist.
	err := os.MkdirAll(persistDir, 0700)
	if err != nil {
		return nil, err
	}
	// Create the logger.
	logger, err := persist.NewFileLogger(filepath.Join(persistDir, "contractor.log"))
	if err != nil {
		return nil, err
	}

	// Create Contractor using production dependencies.
	return newContractor(cs, &walletBridge{w: wallet}, tpool, hdb, newPersist(persistDir), logger)
}

// newContractor creates a Contractor using the provided dependencies.
func newContractor(cs consensusSet, w wallet, tp transactionPool, hdb hostDB, p persister, l *persist.Logger) (*Contractor, error) {
	// Create the Contractor object.
	c := &Contractor{
		cs:      cs,
		hdb:     hdb,
		log:     l,
		persist: p,
		tpool:   tp,
		wallet:  w,

		cachedRevisions: make(map[types.FileContractID]cachedRevision),
		contracts:       make(map[types.FileContractID]modules.RenterContract),
		downloaders:     make(map[types.FileContractID]*hostDownloader),
		editors:         make(map[types.FileContractID]*hostEditor),
		oldContracts:    make(map[types.FileContractID]modules.RenterContract),
		renewedIDs:      make(map[types.FileContractID]types.FileContractID),
		renewing:        make(map[types.FileContractID]bool),
		revising:        make(map[types.FileContractID]bool),
	}

	// Load the prior persistence structures.
	err := c.load()
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	err = cs.ConsensusSetSubscribe(c, c.lastChange)
	if err == modules.ErrInvalidConsensusChangeID {
		// Reset the contractor consensus variables and try rescanning.
		c.blockHeight = 0
		c.lastChange = modules.ConsensusChangeBeginning
		err = cs.ConsensusSetSubscribe(c, c.lastChange)
	}
	if err != nil {
		return nil, errors.New("contractor subscription failed: " + err.Error())
	}

	return c, nil
}
