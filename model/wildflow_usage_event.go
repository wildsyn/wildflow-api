package model

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrWildFlowUsageEventConflict = errors.New("WildFlow usage event identity conflict")
	ErrWildFlowUsageEventUnknown  = errors.New("WildFlow usage event does not match an operation")
	ErrWildFlowUsageEventInvalid  = errors.New("WildFlow usage event is invalid")
)

type WildFlowUsageEvent struct {
	EventID         string    `json:"-" gorm:"type:varchar(128);primaryKey"`
	AggregateType   string    `json:"-" gorm:"type:varchar(32)"`
	AggregateID     string    `json:"-" gorm:"type:varchar(128);index"`
	EventType       string    `json:"-" gorm:"type:varchar(64)"`
	PayloadDigest   string    `json:"-" gorm:"type:char(64)"`
	OperationID     string    `json:"-" gorm:"type:varchar(64);index"`
	JobID           string    `json:"-" gorm:"type:varchar(128);index"`
	AttemptID       string    `json:"-" gorm:"type:varchar(128)"`
	ModelVersionRef string    `json:"-" gorm:"type:varchar(256)"`
	ChannelType     string    `json:"-" gorm:"type:varchar(32)"`
	Kind            string    `json:"-" gorm:"type:varchar(32)"`
	Quantity        int64     `json:"-"`
	Unit            string    `json:"-" gorm:"type:varchar(32)"`
	StartedAt       time.Time `json:"-"`
	EndedAt         time.Time `json:"-"`
	EvidenceRef     string    `json:"-" gorm:"type:varchar(256)"`
	IngestToken     string    `json:"-" gorm:"type:varchar(64)"`
	IngestedAt      time.Time `json:"-" gorm:"precision:3"`
	CreatedTime     int64     `json:"-" gorm:"bigint"`
}

func (event *WildFlowUsageEvent) BeforeCreate(_ *gorm.DB) error {
	if event.IngestToken == "" {
		event.IngestToken = uuid.NewString()
	}
	if event.IngestedAt.IsZero() {
		event.IngestedAt = time.Now().UTC()
	}
	event.IngestedAt = event.IngestedAt.UTC().Truncate(time.Millisecond)
	if event.CreatedTime == 0 {
		event.CreatedTime = event.IngestedAt.Unix()
	}
	return nil
}

func RecordWildFlowUsageEvent(event *WildFlowUsageEvent) (bool, error) {
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		replayed, err := recordWildFlowUsageEventOnce(event)
		if err == nil {
			return replayed, nil
		}
		lastErr = err
		if !retryableWildFlowTransactionContention(err) {
			return false, err
		}
		time.Sleep(time.Duration(attempt+1) * time.Millisecond)
	}

	// Resolve an ambiguous final retry from freshly committed immutable state.
	var operation WildFlowOperation
	operationLoaded := DB != nil && DB.Where(
		"operation_id = ?", event.OperationID,
	).First(&operation).Error == nil
	if operationLoaded && operation.BillingSource == WildFlowBillingSourceTeamTrial &&
		operation.BillingUsageEventID != "" && operation.BillingUsageEventID != event.EventID {
		return false, ErrWildFlowUsageEventConflict
	}
	var persisted WildFlowUsageEvent
	if operationLoaded && DB.Where("event_id = ?", event.EventID).First(&persisted).Error == nil &&
		wildFlowUsageEventIdentityMatches(&persisted, event) &&
		(operation.BillingSource != WildFlowBillingSourceTeamTrial ||
			operation.BillingUsageEventID == event.EventID) {
		return true, nil
	}
	return false, lastErr
}

func recordWildFlowUsageEventOnce(event *WildFlowUsageEvent) (bool, error) {
	if DB == nil || event == nil {
		return false, errors.New("WildFlow usage event database is unavailable")
	}
	if !validWildFlowUsagePayloadDigest(event.PayloadDigest) {
		return false, ErrWildFlowUsageEventInvalid
	}
	// Milliseconds are the narrowest timestamp precision shared by the
	// supported SQLite, MySQL, and PostgreSQL schemas. Canonicalize before the
	// insert so the subsequent primary-key reload can compare exact instants.
	event.StartedAt = event.StartedAt.UTC().Truncate(time.Millisecond)
	event.EndedAt = event.EndedAt.UTC().Truncate(time.Millisecond)
	if event.IngestedAt.IsZero() {
		event.IngestedAt = time.Now().UTC()
	}
	event.IngestedAt = event.IngestedAt.UTC().Truncate(time.Millisecond)
	if event.CreatedTime == 0 {
		event.CreatedTime = event.IngestedAt.Unix()
	}
	replayed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var operation WildFlowOperation
		if err := lockForUpdate(tx).Where("operation_id = ?", event.OperationID).First(&operation).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrWildFlowUsageEventUnknown
			}
			return err
		}
		if operation.JobID != event.JobID || operation.ModelVersionRef != event.ModelVersionRef {
			return ErrWildFlowUsageEventConflict
		}
		if !wildFlowUsageEventMatchesOperation(&operation, event) {
			return ErrWildFlowUsageEventConflict
		}
		var persisted WildFlowUsageEvent
		existingErr := tx.Where("event_id = ?", event.EventID).First(&persisted).Error
		if existingErr == nil {
			if !wildFlowUsageEventIdentityMatches(&persisted, event) {
				return ErrWildFlowUsageEventConflict
			}
			replayed = true
		} else {
			if !errors.Is(existingErr, gorm.ErrRecordNotFound) {
				return existingErr
			}
			result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(event)
			if result.Error != nil {
				return result.Error
			}
			if lookupErr := tx.Where("event_id = ?", event.EventID).First(&persisted).Error; lookupErr != nil {
				return lookupErr
			}
			if !wildFlowUsageEventIdentityMatches(&persisted, event) {
				return ErrWildFlowUsageEventConflict
			}
			replayed = persisted.IngestToken == "" || persisted.IngestToken != event.IngestToken
		}
		if operation.BillingSource == WildFlowBillingSourceTeamTrial {
			if operation.BillingUsageEventID != "" && operation.BillingUsageEventID != event.EventID {
				return ErrWildFlowUsageEventConflict
			}
			if operation.BillingUsageEventID == "" {
				update := tx.Model(&WildFlowOperation{}).
					Where("id = ? AND billing_source = ? AND billing_state = ? AND billing_usage_event_id = ?",
						operation.ID, WildFlowBillingSourceTeamTrial, WildFlowBillingStatePending, "").
					Updates(map[string]any{
						"billing_usage_event_id": event.EventID,
						"updated_time":           time.Now().Unix(),
					})
				if update.Error != nil {
					return update.Error
				}
				if update.RowsAffected != 1 {
					return ErrWildFlowUsageEventConflict
				}
			}
		}
		return nil
	})
	return replayed, err
}

func wildFlowUsageEventIdentityMatches(persisted *WildFlowUsageEvent, incoming *WildFlowUsageEvent) bool {
	if persisted == nil || incoming == nil {
		return false
	}
	return persisted.EventID == incoming.EventID &&
		persisted.AggregateType == incoming.AggregateType &&
		persisted.AggregateID == incoming.AggregateID &&
		persisted.EventType == incoming.EventType &&
		persisted.PayloadDigest == incoming.PayloadDigest &&
		persisted.OperationID == incoming.OperationID &&
		persisted.JobID == incoming.JobID &&
		persisted.AttemptID == incoming.AttemptID &&
		persisted.ModelVersionRef == incoming.ModelVersionRef &&
		persisted.ChannelType == incoming.ChannelType &&
		persisted.Kind == incoming.Kind &&
		persisted.Quantity == incoming.Quantity &&
		persisted.Unit == incoming.Unit &&
		persisted.StartedAt.Equal(incoming.StartedAt) &&
		persisted.EndedAt.Equal(incoming.EndedAt) &&
		persisted.EvidenceRef == incoming.EvidenceRef
}

func validWildFlowUsagePayloadDigest(value string) bool {
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

func wildFlowUsageEventMatchesOperation(operation *WildFlowOperation, event *WildFlowUsageEvent) bool {
	if operation == nil || event == nil {
		return false
	}
	if operation.BillingUsageEventID != "" && operation.BillingUsageEventID != event.EventID {
		return false
	}
	if operation.BillingState == "" || operation.BillingState == WildFlowBillingStatePending {
		return true
	}
	if event.Quantity != operation.BillingBillableUnits {
		return false
	}
	switch operation.BillingUnit {
	case "10k_characters":
		return event.Kind == "characters" && event.Unit == "character"
	case "image":
		return event.Kind == "images" && event.Unit == "image"
	default:
		return false
	}
}
