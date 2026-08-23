package model

import (
	"time"

	"gorm.io/gorm"
)

// WildFlowBillingLogEntry is the canonical, concurrency-safe billing audit
// record. The generic Log row is a UI projection and is emitted only by the
// caller that wins this table's composite key.
type WildFlowBillingLogEntry struct {
	OperationID              string `json:"-" gorm:"type:varchar(64);primaryKey"`
	LogType                  int    `json:"-" gorm:"primaryKey;autoIncrement:false"`
	UsageEventID             string `json:"-" gorm:"type:varchar(128);index"`
	BillingSource            string `json:"-" gorm:"type:varchar(32)"`
	BillingQuota             int    `json:"-"`
	BillingCurrency          string `json:"-" gorm:"type:varchar(8)"`
	BillingAmountMicros      int64  `json:"-" gorm:"bigint"`
	Content                  string `json:"-" gorm:"type:varchar(255)"`
	ProjectionState          string `json:"-" gorm:"type:varchar(32);index"`
	ProjectionAttempts       int    `json:"-"`
	ProjectionLastError      string `json:"-" gorm:"type:varchar(64)"`
	ProjectionClaimToken     string `json:"-" gorm:"type:varchar(64)"`
	ProjectionLeaseExpiresAt int64  `json:"-" gorm:"bigint;index"`
	ProjectedTime            int64  `json:"-" gorm:"bigint"`
	CreatedTime              int64  `json:"-" gorm:"bigint"`
}

const (
	WildFlowBillingProjectionPending     = "pending"
	WildFlowBillingProjectionProjecting  = "projecting"
	WildFlowBillingProjectionFailed      = "failed"
	WildFlowBillingProjectionProjected   = "projected"
	WildFlowBillingProjectionNotRequired = "not_required"
)

func (entry *WildFlowBillingLogEntry) BeforeCreate(_ *gorm.DB) error {
	if entry.CreatedTime == 0 {
		entry.CreatedTime = time.Now().Unix()
	}
	if entry.ProjectionState == "" {
		entry.ProjectionState = WildFlowBillingProjectionPending
	}
	return nil
}
