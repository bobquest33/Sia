package contractmanager

// TODO: Lock is being held during disk write in managedAddSector.

import (
	"encoding/binary"
	"errors"

	"github.com/NebulousLabs/Sia/build"
	"github.com/NebulousLabs/Sia/crypto"
	"github.com/NebulousLabs/Sia/modules"
)

// commitUpdateSector will commit a sector update to the contract manager,
// writing in metadata and usage info if the sector still exists, and deleting
// the usage info if the sector does not exist. The update is idempotent.
func (wal *writeAheadLog) commitUpdateSector(su sectorUpdate) {
	// Grab the usage flag, as it will need to be updated.
	usageElement := wal.cm.storageFolders[su.Folder].Usage[su.Index/storageFolderGranularity]
	bitIndex := su.Index % storageFolderGranularity

	// If the sector is being cleaned from disk, unset the usage flag. No need
	// to update the metadata, the contractor now sees it as garbage data
	// anyway.
	if su.Count == 0 {
		usageElement = usageElement & (^(1 << bitIndex))
		wal.cm.storageFolders[su.Folder].Usage[su.Index/storageFolderGranularity] = usageElement
		return
	}

	// If the sector is not being purged, set the usage flag.
	usageElement = usageElement | (1 << bitIndex)
	wal.cm.storageFolders[su.Folder].Usage[su.Index/storageFolderGranularity] = usageElement

	// Write the updated sector metadata to disk. The sector itself will
	// already have been written to disk and synced.
	wal.writeSectorMetadata(su)
}

// managedAddSector is a WAL operation to add a sector to the contract manager.
func (wal *writeAheadLog) managedAddSector(id sectorID, data []byte) error {
	var syncChan chan struct{}
	err := func() error {
		wal.mu.Lock()
		defer wal.mu.Unlock()

		// Create and fill out the sectorUpdate object.
		su := sectorUpdate{
			ID: id,
		}

		// Grab the number of virtual sectors that have been committed with
		// this root.
		location, exists := wal.cm.sectorLocations[id]
		if exists {
			// Check whether the maximum number of virtual sectors has been
			// reached.
			if location.count == 65535 {
				return errMaxVirtualSectors
			}

			// Update the location count to indicate that a sector has been
			// added.
			location.count += 1
			wal.cm.sectorLocations[id] = location

			// Fill out the sectorUpdate object so that it can be added to the
			// WAL.
			su.Count = location.count
			su.Folder = location.storageFolder
			su.Index = location.index
		} else {
			// Sanity check - data should have modules.SectorSize bytes.
			if uint64(len(data)) != modules.SectorSize {
				wal.cm.log.Critical("sector has the wrong size", modules.SectorSize, len(data))
				return errors.New("malformed sector")
			}

			// Find a committed storage folder that has enough space to receive
			// this sector. Keep trying new storage folders if some return
			// errors during disk operations.
			storageFolders := wal.cm.storageFolderSlice()
			sf := new(storageFolder)
			var sectorIndex uint32
			var storageFolderIndex int
			for {
				sf, storageFolderIndex = emptiestStorageFolder(storageFolders)
				if sf == nil {
					// None of the storage folders have enough room to house the
					// sector.
					return errInsufficientStorageForSector
				}
				err := func() error {
					// Find a location for the sector within the file using the Usage
					// field.
					var err error
					sectorIndex, err = randFreeSector(sf.Usage)
					if err != nil {
						wal.cm.log.Critical("a storage folder with full usage was returned from emptiestStorageFolder")
						return err
					}
					// TODO: Need to flip the usage immediately after
					// requesting the sector, and to un-flip it if the Write
					// fails.

					// Write the new sector to disk. Any data existing in this location
					// on disk is either garbage or is from a sector that has been
					// removed through a successfully committed remove operation - no
					// risk of corruption to write immediately.
					//
					// If the commitment to update the metadata fails, the host will
					// never know that the sector existed on-disk and will treat it as
					// garbage data - which does not threaten consistency.
					_, err = sf.sectorFile.Seek(int64(modules.SectorSize)*int64(sectorIndex), 0)
					if err != nil {
						wal.cm.log.Println("ERROR: unable to seek to sector data when adding sector")
						sf.failedWrites += 1
						return errDiskTrouble
					}
					_, err = sf.sectorFile.Write(data)
					if err != nil {
						wal.cm.log.Println("ERROR: unable to write sector data when adding sector")
						sf.failedWrites += 1
						return errDiskTrouble
					}
					return nil
				}()
				if err == nil {
					// Sector added to a storage folder successfully.
					break
				}
				// Sector not added to storage folder successfully, remove this
				// stoage folder from the list of storage folders, and try the
				// next one.
				storageFolders = append(storageFolders[:storageFolderIndex], storageFolders[storageFolderIndex+1:]...)
			}

			// Update the state to reflect the new sector.
			wal.cm.sectorLocations[id] = sectorLocation{
				index:         sectorIndex,
				storageFolder: sf.Index,
				count:         1,
			}
			sf.sectors += 1

			// Update the usage field in the storage folder.
			usageElement := sf.Usage[sectorIndex/storageFolderGranularity]
			bitIndex := sectorIndex % storageFolderGranularity
			usageElement = usageElement | (1 << bitIndex)
			sf.Usage[sectorIndex/storageFolderGranularity] = usageElement

			// Fill out the sectorUpdate fields.
			su.Count = 1
			su.Folder = sf.Index
			su.Index = sectorIndex
		}

		// Write the sector metadata to disk.
		err := wal.writeSectorMetadata(su)
		if err != nil {
			return build.ExtendErr("unable to write sector metadata during addSector call", err)
		}

		// Add a change to the WAL to commit this sector to the provided index.
		err = wal.appendChange(stateChange{
			SectorUpdates: []sectorUpdate{su},
		})
		if err != nil {
			return build.ExtendErr("failed to add a state change", err)
		}

		// Grab the synchronization channel so that we know when the sector add
		// has been committed.
		syncChan = wal.syncChan
		return nil
	}()
	if err != nil {
		return build.ExtendErr("verification failed:", err)
	}

	// Only return after the commitment has succeeded fully.
	<-syncChan
	return nil
}

// managedDeleteSector will delete a sector (physical) from the contract manager.
func (wal *writeAheadLog) managedDeleteSector(id sectorID) error {
	// Create and fill out the sectorUpdate object.
	su := sectorUpdate{
		ID: id,
	}
	var syncChan chan struct{}
	err := func() error {
		wal.mu.Lock()
		defer wal.mu.Unlock()

		// Grab the number of virtual sectors that have been committed with
		// this root.
		location, exists := wal.cm.sectorLocations[id]
		if !exists {
			return errSectorNotFound
		}
		// Delete the sector from the sector locations map.
		delete(wal.cm.sectorLocations, id)

		// Fill out the sectorUpdate object so that it can be added to the
		// WAL.
		su.Count = 0 // This function is only being called if we want to set the count to zero.
		su.Folder = location.storageFolder
		su.Index = location.index

		// Inform the WAL of the sector update.
		err := wal.appendChange(stateChange{
			SectorUpdates: []sectorUpdate{su},
		})
		if err != nil {
			return build.ExtendErr("failed to add a state change", err)
		}

		// Grab the sync channel to know when the update has been durably
		// committed.
		syncChan = wal.syncChan
		return nil
	}()
	if err != nil {
		return build.ExtendErr("cannot delete sector:", err)
	}

	// Only return after the commitment has succeeded fully.
	<-syncChan

	// Only update the usage after the sector delete has been committed to disk
	// fully.
	//
	// The usage is not updated until after the commit has completed to prevent
	// the actual sector data from being overwritten in the event of unclean
	// shutdown.
	wal.mu.Lock()
	defer wal.mu.Unlock()
	usageElement := wal.cm.storageFolders[su.Folder].Usage[su.Index/storageFolderGranularity]
	bitIndex := su.Index % storageFolderGranularity
	usageElement = usageElement & (^(1 << bitIndex))
	wal.cm.storageFolders[su.Folder].Usage[su.Index/storageFolderGranularity] = usageElement
	wal.cm.storageFolders[su.Folder].sectors--
	return nil
}

// managedRemoveSector will remove a sector (virtual or physical) from the
// contract manager.
func (wal *writeAheadLog) managedRemoveSector(id sectorID) error {
	// Create and fill out the sectorUpdate object.
	su := sectorUpdate{
		ID: id,
	}
	var syncChan chan struct{}
	err := func() error {
		wal.mu.Lock()
		defer wal.mu.Unlock()

		// Check that the storage folder is not currently undergoing a move
		// operation.

		// Grab the number of virtual sectors that have been committed with
		// this root.
		location, exists := wal.cm.sectorLocations[id]
		if !exists {
			return errSectorNotFound
		}
		location.count--

		// Fill out the sectorUpdate object so that it can be added to the
		// WAL.
		su.Count = location.count
		su.Folder = location.storageFolder
		su.Index = location.index

		// Inform the WAL of the sector update.
		err := wal.appendChange(stateChange{
			SectorUpdates: []sectorUpdate{su},
		})
		if err != nil {
			return build.ExtendErr("failed to add a state change", err)
		}

		// Update the in memory representation of the sector (except the
		// usage), and write the new metadata to disk if needed.
		//
		// The usage is not updated until after the commit has completed to
		// prevent the actual sector data from being overwritten in the event
		// of unclean shutdown.
		if su.Count != 0 {
			wal.cm.sectorLocations[id] = location
			err = wal.writeSectorMetadata(su)
			if err != nil {
				return build.ExtendErr("failed to write sector metadata", err)
			}
		} else {
			delete(wal.cm.sectorLocations, id)
		}

		// Grab the sync channel to know when the update has been durably
		// committed.
		syncChan = wal.syncChan
		return nil
	}()
	if err != nil {
		return build.ExtendErr("cannot remove sector:", err)
	}
	<-syncChan

	// Only update the usage after the sector removal has been committed to
	// disk entirely.
	//
	// The usage is not updated until after the commit has completed to prevent
	// the actual sector data from being overwritten in the event of unclean
	// shutdown.
	if su.Count == 0 {
		wal.mu.Lock()
		defer wal.mu.Unlock()
		usageElement := wal.cm.storageFolders[su.Folder].Usage[su.Index/storageFolderGranularity]
		bitIndex := su.Index % storageFolderGranularity
		usageElement = usageElement & (^(1 << bitIndex))
		wal.cm.storageFolders[su.Folder].Usage[su.Index/storageFolderGranularity] = usageElement
		wal.cm.storageFolders[su.Folder].sectors--
	}
	return nil
}

// writeSectorMetadata will take a sector update and write the related metadata
// to disk.
func (wal *writeAheadLog) writeSectorMetadata(su sectorUpdate) error {
	sf := wal.cm.storageFolders[su.Folder]
	writeData := make([]byte, sectorMetadataDiskSize)
	copy(writeData, su.ID[:])
	binary.LittleEndian.PutUint16(writeData[12:], su.Count)
	_, err := sf.metadataFile.Seek(sectorMetadataDiskSize*int64(su.Index), 0)
	if err != nil {
		wal.cm.log.Println("ERROR: unable to seek to sector metadata when adding sector")
		sf.failedWrites += 1
		return err
	}
	_, err = sf.metadataFile.Write(writeData)
	if err != nil {
		wal.cm.log.Println("ERROR: unable to write sector metadata when adding sector")
		sf.failedWrites += 1
		return err
	}
	// Writes were successful, update the storage folder stats.
	sf.successfulWrites += 1
	return nil
}

// AddSector will add a sector to the contract manager.
func (cm *ContractManager) AddSector(root crypto.Hash, sectorData []byte) error {
	return cm.wal.managedAddSector(cm.managedSectorID(root), sectorData)
}

// DeleteSector will delete a sector from the contract manager. If multiple
// copies of the sector exist, all of them will be removed. This should only be
// used to remove offensive data, as it will cause corruption in the contract
// manager. This corruption puts the contract manager at risk of failing
// storage proofs. If the amount of data removed is small, the risk is small.
// This operation will not destabilize the contract manager.
func (cm *ContractManager) DeleteSector(root crypto.Hash) error {
	return cm.wal.managedDeleteSector(cm.managedSectorID(root))
}

// RemoveSector will remove a sector from the contract manager. If multiple
// copies of the sector exist, only one will be removed.
func (cm *ContractManager) RemoveSector(root crypto.Hash) error {
	return cm.wal.managedRemoveSector(cm.managedSectorID(root))
}