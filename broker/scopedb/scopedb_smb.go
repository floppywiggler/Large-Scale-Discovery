/*
* Large-Scale Discovery, a network scanning solution for information gathering in large IT/OT network environments.
*
* Copyright (c) Siemens AG, 2016-2021.
*
* This work is licensed under the terms of the MIT license. For a copy, see the LICENSE file in the top-level
* directory or visit <https://opensource.org/licenses/MIT>.
*
 */

package scopedb

import (
	"database/sql"
	"fmt"
	"github.com/jackc/pgconn"
	"github.com/siemens/GoScans/smb"
	scanUtils "github.com/siemens/GoScans/utils"
	"gorm.io/gorm"
	managerdb "large-scale-discovery/manager/database"
	"strings"
	"sync"
	"time"
)

// PrepareSmbResult creates an info entry in the database to indicate an active process.
func PrepareSmbResult(
	logger scanUtils.Logger,
	scanScope *managerdb.T_scan_scope,
	idsTDiscoveryService []uint64,
	scanStarted sql.NullTime,
	scanIp string,
	scanHostname string,
	wg *sync.WaitGroup,
) {

	// Notify the WaitGroup that we're done
	defer wg.Done()

	// Log action
	logger.Debugf("Preparing SMB info entry.")

	// Return if there were no results to be created
	if len(idsTDiscoveryService) == 0 {
		logger.Debugf("No discovery services for SMB info entries provided.")
		return
	}

	// Open scope's database
	scopeDb, errHandle := managerdb.GetScopeDbHandle(logger, scanScope)
	if errHandle != nil {
		logger.Errorf("Could not open scope database: %s", errHandle)
		return
	}

	// Iterate service IDs to create database info entries for
	infoEntries := make([]managerdb.T_smb, 0, len(idsTDiscoveryService))
	for _, idTDiscoveryService := range idsTDiscoveryService {

		// Prepare info entry
		infoEntries = append(infoEntries, managerdb.T_smb{
			IdTDiscoveryService: idTDiscoveryService,
			ColumnsScan: managerdb.ColumnsScan{
				ScanStarted:  scanStarted,
				ScanFinished: sql.NullTime{Valid: false},
				ScanStatus:   scanUtils.StatusRunning,
				ScanIp:       scanIp,
				ScanHostname: scanHostname,
			},
		})
	}

	// Create info entries, if not yet existing. It might exist from a previous scan attempt (if the scan crashed).
	// This check which is applied here is specific to postgres. Use a new gorm session and force a limit on how
	// many Entries can be batched, as we otherwise might exceed PostgreSQLs limit of 65535 parameters
	errDb := scopeDb.
		Session(&gorm.Session{CreateBatchSize: managerdb.MaxBatchSizeSmb}).
		Create(&infoEntries).Error
	if errCreate, ok := errDb.(*pgconn.PgError); ok && errCreate.Code == "23505" { // Code for unique constraint violation

		// Fall back to inserting the entries one by one to ensure as many entries as possible being added to the db
		for _, entry := range infoEntries {
			errDb2 := scopeDb.Create(&entry).Error
			if errCreate2, ok2 := errDb2.(*pgconn.PgError); ok2 && errCreate2.Code == "23505" { // Code for unique constraint violation
				logger.Debugf("SMB info entry '%d' already existing.", entry.IdTDiscoveryService)
			} else if errDb2 != nil {
				logger.Errorf("SMB info entry '%d' could not be created: %s", entry.IdTDiscoveryService, errDb2)
			}
		}
	} else if errDb != nil {
		logger.Errorf("SMB info entries could not be created: %s", errDb)
	}
}

// SaveSmbResult parses a result and adds its data into the database. Furthermore, it sets the "scan_finished"
// timestamp to indicate a completed process.
func SaveSmbResult(
	logger scanUtils.Logger,
	scanScope *managerdb.T_scan_scope,
	idTDiscoveryService uint64, // T_discovery_services ID this result belongs to
	result *smb.Result,
) error {

	// Log action
	logger.Debugf("Saving SMB scan result.")

	// Open scope's database
	scopeDb, errHandle := managerdb.GetScopeDbHandle(logger, scanScope)
	if errHandle != nil {
		return fmt.Errorf("could not open scope database: %s", errHandle)
	}

	// Start transaction on the scoped db. The new Transaction function will commit if the provided function
	// returns nil and rollback if an error is returned.
	return scopeDb.Transaction(func(txScopeDb *gorm.DB) error {
		// Check the condition of the info entry stored in the database. It got created the first time the target was
		// taken from the cache (info entry should never be removed from the scope database again!). However,
		// 		A) the scan could have crashed before and been restarted by the broker after its timeout
		// 		B) the broker could have been down for a prolonged time, restarting the scan after an assumed timeout.
		// 		   Both scan instances might deliver results in arbitrary order.
		//
		// Case A: The scan_finished timestamp is not set, because this is the first scan instances delivering results
		// 		    => Save results, set scan_finished timestamp
		// Case B: The scan_finished timestamp is already set, because this is a later scan instance delivering results
		// 			=> Discard results (to prevent later manipulation by client), but update scan_finished timestamp (to
		// 			   allow users proper investigations in case of IDS alerts).

		// Prepare query result
		var infoEntry = managerdb.T_smb{}
		var infoEntryCount int64

		// Query info entry
		errDb := txScopeDb.Where("id_t_discovery_service = ?", idTDiscoveryService).
			First(&infoEntry).
			Count(&infoEntryCount).Error
		if errDb != nil {
			return fmt.Errorf("could not find associated info entry in scope db: %s", errDb)
		}

		// Check if info entry was found
		if infoEntryCount != 1 {
			logger.Infof("Dropping SMB result of vanished discovery result.") // Scan database might have been reset or cleaned up
			return nil
		}

		// Add scan results, if this is the first result delivered (scan_finished not yet set)
		if infoEntry.ScanFinished.Valid == false {

			// Add file system crawler information
			errAdd := addSmbResults(txScopeDb, infoEntry.Id, idTDiscoveryService, result)
			if errAdd != nil {
				return fmt.Errorf("could not insert result into scope db: %s", errAdd)
			}

			// Add scan status to info entry and additional info to the info column
			infoEntry.ScanStatus = result.Status
			infoEntry.FoldersReadable = result.FoldersReadable
			infoEntry.FilesReadable = result.FilesReadable
			infoEntry.FilesWritable = result.FilesWritable
		}

		// Update finished timestamp. Set or update it to the current time.
		infoEntry.ScanFinished = sql.NullTime{Time: time.Now(), Valid: true}

		// Update db with changes
		errDb = txScopeDb.Model(&infoEntry).
			Where("id_t_discovery_service = ?", idTDiscoveryService).
			Updates(infoEntry).Error
		if errDb != nil {
			return fmt.Errorf("could not update entry in scope db: %s", errDb)
		}

		// Return nil as everything went fine
		return nil
	})
}

func addSmbResults(txScopeDb *gorm.DB, tSmbInfoId uint64, idTDiscoveryService uint64, result *smb.Result) error {

	// Append scan results
	dataEntries := make([]managerdb.T_smb_file, 0, len(result.Data))
	for _, item := range result.Data {

		// Prepare data entry
		dataEntries = append(dataEntries, managerdb.T_smb_file{
			IdTDiscoveryService: idTDiscoveryService,
			IdTSmb:              tSmbInfoId,
			Share:               item.Share,
			Path:                item.Path,
			Name:                item.Name,
			Extension:           item.Extension,
			Mime:                item.Mime,
			Readable:            item.Readable,
			Writable:            item.Writable,
			SizeKb:              item.SizeKb,
			LastModified:        sql.NullTime{Time: item.LastModified, Valid: item.LastModified != time.Time{}},
			Depth:               item.Depth,
			Properties:          strings.Join(item.Properties, DbValueSeparator),
			IsSymlink:           item.IsSymlink,
			IsDfs:               item.IsDfs,
		})
	}

	// Insert data into db. Empty slices would result in an error and we don't consider an empty slice an error, as
	// there might simply be no results
	if len(dataEntries) > 0 {

		// Use a new gorm session and force a limit on how many Entries can be batched, as we otherwise might
		// exceed PostgreSQLs limit of 65535 parameters
		errDb := txScopeDb.
			Session(&gorm.Session{CreateBatchSize: managerdb.MaxBatchSizeSmbFile}).
			Create(&dataEntries).Error
		if errDb != nil {
			return errDb
		}
	}

	// Return nil as everything went fine
	return nil
}
