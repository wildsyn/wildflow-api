package model

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrWildFlowJourneyEvidenceInvalid  = errors.New("WildFlow journey evidence is invalid")
	ErrWildFlowJourneyEvidenceNotFound = errors.New("WildFlow journey evidence is incomplete")
	ErrWildFlowJourneyEvidenceConflict = errors.New("WildFlow journey evidence conflicts")
	errWildFlowJourneyConcurrentInsert = errors.New("WildFlow journey receipt was inserted concurrently")
)

const wildFlowPublicJourneyReceiptMaxBytes = 65535

type WildFlowArtifactDownloadReceipt struct {
	ID                int64     `json:"-" gorm:"primaryKey"`
	OperationID       string    `json:"-" gorm:"type:varchar(64);uniqueIndex:idx_wildflow_download_operation_artifact,priority:1;index"`
	JobID             string    `json:"-" gorm:"type:varchar(128);index"`
	ArtifactID        string    `json:"-" gorm:"type:varchar(128);uniqueIndex:idx_wildflow_download_operation_artifact,priority:2;index"`
	UserID            int       `json:"-" gorm:"index"`
	TenantRefSHA256   string    `json:"-" gorm:"type:char(64)"`
	ArtifactMediaType string    `json:"-" gorm:"type:varchar(100)"`
	ArtifactSizeBytes int64     `json:"-" gorm:"bigint"`
	ArtifactSHA256    string    `json:"-" gorm:"type:char(64)"`
	InsertToken       string    `json:"-" gorm:"type:varchar(64)"`
	CompletedAt       time.Time `json:"-" gorm:"precision:3"`
}

func (receipt *WildFlowArtifactDownloadReceipt) BeforeCreate(_ *gorm.DB) error {
	if receipt.InsertToken == "" {
		receipt.InsertToken = uuid.NewString()
	}
	if receipt.CompletedAt.IsZero() {
		receipt.CompletedAt = time.Now().UTC()
	}
	receipt.CompletedAt = receipt.CompletedAt.UTC().Truncate(time.Millisecond)
	return nil
}

func RecordWildFlowArtifactDownloadReceipt(
	receipt *WildFlowArtifactDownloadReceipt,
) (*WildFlowArtifactDownloadReceipt, bool, error) {
	if DB == nil || !validWildFlowArtifactDownloadReceipt(receipt) {
		return nil, false, ErrWildFlowJourneyEvidenceInvalid
	}
	receipt.InsertToken = uuid.NewString()
	receipt.CompletedAt = receipt.CompletedAt.UTC().Truncate(time.Millisecond)
	var persisted WildFlowArtifactDownloadReceipt
	replayed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "operation_id"}, {Name: "artifact_id"}},
			DoNothing: true,
		}).Create(receipt)
		if result.Error != nil {
			return result.Error
		}
		if err := tx.Where(
			"operation_id = ? AND artifact_id = ?", receipt.OperationID, receipt.ArtifactID,
		).First(&persisted).Error; err != nil {
			return err
		}
		if !wildFlowArtifactDownloadReceiptIdentityMatches(&persisted, receipt) {
			return ErrWildFlowJourneyEvidenceConflict
		}
		replayed = persisted.InsertToken == "" || persisted.InsertToken != receipt.InsertToken
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return &persisted, replayed, nil
}

type WildFlowJourneyEvidence struct {
	Operation       WildFlowOperation
	UsageEvent      WildFlowUsageEvent
	DownloadReceipt WildFlowArtifactDownloadReceipt
}

type WildFlowPublicJourneyReceiptRecord struct {
	ID               int64     `json:"-" gorm:"primaryKey"`
	OperationID      string    `json:"-" gorm:"type:varchar(64);uniqueIndex:idx_wildflow_public_journey_operation"`
	JobID            string    `json:"-" gorm:"type:varchar(128);index"`
	ArtifactID       string    `json:"-" gorm:"type:varchar(128);index"`
	ReceiptJSON      string    `json:"-" gorm:"type:text"`
	ReceiptSHA256    string    `json:"-" gorm:"type:char(64)"`
	ReceiptCreatedAt time.Time `json:"-" gorm:"precision:3"`
}

type WildFlowJourneyReceiptMaterial struct {
	ReceiptJSON      string
	ReceiptSHA256    string
	ReceiptCreatedAt time.Time
}

type WildFlowJourneyReceiptBuilder func(
	evidence *WildFlowJourneyEvidence,
) (*WildFlowJourneyReceiptMaterial, error)

func LoadWildFlowJourneyEvidence(
	ctx context.Context,
	operationID string,
	jobID string,
	artifactID string,
) (*WildFlowJourneyEvidence, error) {
	if DB == nil || !validWildFlowJourneyEvidenceID(operationID, 64) ||
		!validWildFlowJourneyEvidenceID(jobID, 128) ||
		!validWildFlowJourneyEvidenceID(artifactID, 128) {
		return nil, ErrWildFlowJourneyEvidenceInvalid
	}
	evidence := &WildFlowJourneyEvidence{}
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return loadWildFlowJourneyEvidenceTx(tx, operationID, jobID, artifactID, evidence)
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	return evidence, nil
}

func LoadOrCreateWildFlowPublicJourneyReceipt(
	ctx context.Context,
	operationID string,
	jobID string,
	artifactID string,
	builder WildFlowJourneyReceiptBuilder,
) (*WildFlowPublicJourneyReceiptRecord, error) {
	if DB == nil || builder == nil || !validWildFlowJourneyEvidenceID(operationID, 64) ||
		!validWildFlowJourneyEvidenceID(jobID, 128) ||
		!validWildFlowJourneyEvidenceID(artifactID, 128) {
		return nil, ErrWildFlowJourneyEvidenceInvalid
	}
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		var result WildFlowPublicJourneyReceiptRecord
		err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var existing WildFlowPublicJourneyReceiptRecord
			existingErr := tx.Where("operation_id = ?", operationID).First(&existing).Error
			if existingErr == nil {
				if existing.JobID != jobID || existing.ArtifactID != artifactID ||
					!validWildFlowPublicJourneyReceiptRecord(&existing) {
					return ErrWildFlowJourneyEvidenceConflict
				}
				result = existing
				return nil
			}
			if !errors.Is(existingErr, gorm.ErrRecordNotFound) {
				return existingErr
			}
			evidence := &WildFlowJourneyEvidence{}
			if err := loadWildFlowJourneyEvidenceTx(tx, operationID, jobID, artifactID, evidence); err != nil {
				return err
			}
			material, err := builder(evidence)
			if err != nil {
				return err
			}
			if material == nil {
				return ErrWildFlowJourneyEvidenceInvalid
			}
			candidate := WildFlowPublicJourneyReceiptRecord{
				OperationID: operationID, JobID: jobID, ArtifactID: artifactID,
				ReceiptJSON: material.ReceiptJSON, ReceiptSHA256: material.ReceiptSHA256,
				ReceiptCreatedAt: material.ReceiptCreatedAt.UTC().Truncate(time.Millisecond),
			}
			if !validWildFlowPublicJourneyReceiptRecord(&candidate) {
				return ErrWildFlowJourneyEvidenceInvalid
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "operation_id"}}, DoNothing: true,
			}).Create(&candidate).Error; err != nil {
				return err
			}
			if err := tx.Where("operation_id = ?", operationID).First(&result).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return errWildFlowJourneyConcurrentInsert
				}
				return err
			}
			if result.JobID != jobID || result.ArtifactID != artifactID ||
				!validWildFlowPublicJourneyReceiptRecord(&result) {
				return ErrWildFlowJourneyEvidenceConflict
			}
			return nil
		}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
		if err == nil {
			return &result, nil
		}
		lastErr = err
		if !retryableWildFlowTransactionContention(err) {
			return nil, err
		}
		time.Sleep(time.Duration(attempt+1) * time.Millisecond)
	}
	// A concurrent winner may have committed immediately after the last
	// repeatable-read snapshot failed. A final fresh read can safely replay
	// that immutable row; it never synthesizes a receipt from live evidence.
	if existing, err := LoadWildFlowPublicJourneyReceiptRecord(ctx, operationID); err == nil {
		if existing.JobID != jobID || existing.ArtifactID != artifactID {
			return nil, ErrWildFlowJourneyEvidenceConflict
		}
		return existing, nil
	} else if errors.Is(err, ErrWildFlowJourneyEvidenceConflict) {
		return nil, err
	}
	return nil, lastErr
}

func LoadWildFlowPublicJourneyReceiptRecord(
	ctx context.Context,
	operationID string,
) (*WildFlowPublicJourneyReceiptRecord, error) {
	if DB == nil || !validWildFlowJourneyEvidenceID(operationID, 64) {
		return nil, ErrWildFlowJourneyEvidenceInvalid
	}
	var record WildFlowPublicJourneyReceiptRecord
	if err := DB.WithContext(ctx).Where("operation_id = ?", operationID).First(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrWildFlowJourneyEvidenceNotFound
		}
		return nil, err
	}
	if !validWildFlowPublicJourneyReceiptRecord(&record) {
		return nil, ErrWildFlowJourneyEvidenceConflict
	}
	return &record, nil
}

func loadWildFlowJourneyEvidenceTx(
	tx *gorm.DB,
	operationID string,
	jobID string,
	artifactID string,
	evidence *WildFlowJourneyEvidence,
) error {
	if err := tx.Where(
		"operation_id = ? AND job_id = ?", operationID, jobID,
	).First(&evidence.Operation).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrWildFlowJourneyEvidenceNotFound
		}
		return err
	}
	var usageEvents []WildFlowUsageEvent
	if err := tx.Where(
		"operation_id = ? AND job_id = ? AND model_version_ref = ?",
		operationID,
		jobID,
		evidence.Operation.ModelVersionRef,
	).Order("ingested_at asc, event_id asc").Limit(2).Find(&usageEvents).Error; err != nil {
		return err
	}
	if len(usageEvents) == 0 {
		return ErrWildFlowJourneyEvidenceNotFound
	}
	if len(usageEvents) != 1 || evidence.Operation.BillingUsageEventID == "" ||
		usageEvents[0].EventID != evidence.Operation.BillingUsageEventID {
		return ErrWildFlowJourneyEvidenceConflict
	}
	evidence.UsageEvent = usageEvents[0]
	if err := tx.Where(
		"operation_id = ? AND job_id = ? AND artifact_id = ?",
		operationID,
		jobID,
		artifactID,
	).First(&evidence.DownloadReceipt).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrWildFlowJourneyEvidenceNotFound
		}
		return err
	}
	return nil
}

func validWildFlowPublicJourneyReceiptRecord(record *WildFlowPublicJourneyReceiptRecord) bool {
	if record == nil || !validWildFlowJourneyEvidenceID(record.OperationID, 64) ||
		!validWildFlowJourneyEvidenceID(record.JobID, 128) ||
		!validWildFlowJourneyEvidenceID(record.ArtifactID, 128) ||
		record.ReceiptJSON == "" || len(record.ReceiptJSON) > wildFlowPublicJourneyReceiptMaxBytes ||
		!strings.HasSuffix(record.ReceiptJSON, "\n") ||
		!validWildFlowJourneySHA256(record.ReceiptSHA256) || record.ReceiptCreatedAt.IsZero() {
		return false
	}
	digest := sha256.Sum256([]byte(record.ReceiptJSON))
	return hex.EncodeToString(digest[:]) == record.ReceiptSHA256
}

func validWildFlowArtifactDownloadReceipt(receipt *WildFlowArtifactDownloadReceipt) bool {
	return receipt != nil &&
		validWildFlowJourneyEvidenceID(receipt.OperationID, 64) &&
		validWildFlowJourneyEvidenceID(receipt.JobID, 128) &&
		validWildFlowJourneyEvidenceID(receipt.ArtifactID, 128) &&
		receipt.UserID > 0 &&
		validWildFlowJourneySHA256(receipt.TenantRefSHA256) &&
		receipt.ArtifactMediaType != "" && len(receipt.ArtifactMediaType) <= 100 &&
		strings.TrimSpace(receipt.ArtifactMediaType) == receipt.ArtifactMediaType &&
		receipt.ArtifactSizeBytes > 0 &&
		validWildFlowJourneySHA256(receipt.ArtifactSHA256) &&
		!receipt.CompletedAt.IsZero()
}

func wildFlowArtifactDownloadReceiptIdentityMatches(
	persisted *WildFlowArtifactDownloadReceipt,
	incoming *WildFlowArtifactDownloadReceipt,
) bool {
	return persisted != nil && incoming != nil &&
		persisted.OperationID == incoming.OperationID &&
		persisted.JobID == incoming.JobID &&
		persisted.ArtifactID == incoming.ArtifactID &&
		persisted.UserID == incoming.UserID &&
		persisted.TenantRefSHA256 == incoming.TenantRefSHA256 &&
		persisted.ArtifactMediaType == incoming.ArtifactMediaType &&
		persisted.ArtifactSizeBytes == incoming.ArtifactSizeBytes &&
		persisted.ArtifactSHA256 == incoming.ArtifactSHA256
}

func validWildFlowJourneyEvidenceID(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && strings.TrimSpace(value) == value &&
		!strings.ContainsAny(value, "\x00\r\n")
}

func validWildFlowJourneySHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for index := range value {
		if (value[index] < '0' || value[index] > '9') && (value[index] < 'a' || value[index] > 'f') {
			return false
		}
	}
	return true
}

func retryableWildFlowTransactionContention(err error) bool {
	if errors.Is(err, errWildFlowJourneyConcurrentInsert) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "sqlstate 40001") ||
		strings.Contains(message, "could not serialize access") ||
		strings.Contains(message, "deadlock found") ||
		strings.Contains(message, "database is locked") ||
		strings.Contains(message, "database table is locked")
}
