package service

import (
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

const (
	wildFlowSubmissionLeaseDefault       = 60 * time.Second
	wildFlowSubmissionLeaseMinimum       = 45 * time.Second
	wildFlowSubmissionLeaseMaximum       = 5 * time.Minute
	wildFlowSubmissionRetryWindowDefault = 15 * time.Minute
	wildFlowSubmissionRetryWindowMinimum = time.Minute
	wildFlowSubmissionRetryWindowMaximum = 24 * time.Hour
)

func wildFlowSubmissionLeaseDuration() time.Duration {
	return boundedWildFlowDuration(
		"WILDFLOW_SUBMISSION_LEASE_SECONDS",
		wildFlowSubmissionLeaseDefault,
		wildFlowSubmissionLeaseMinimum,
		wildFlowSubmissionLeaseMaximum,
	)
}

func wildFlowSubmissionRetryWindow() time.Duration {
	return boundedWildFlowDuration(
		"WILDFLOW_SUBMISSION_RETRY_WINDOW_SECONDS",
		wildFlowSubmissionRetryWindowDefault,
		wildFlowSubmissionRetryWindowMinimum,
		wildFlowSubmissionRetryWindowMaximum,
	)
}

func boundedWildFlowDuration(name string, fallback time.Duration, minimum time.Duration, maximum time.Duration) time.Duration {
	seconds := common.GetEnvOrDefault(name, int(fallback/time.Second))
	if seconds < int(minimum/time.Second) || seconds > int(maximum/time.Second) {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

func WildFlowSubmissionLeaseDeadline() int64 {
	return time.Now().Add(wildFlowSubmissionLeaseDuration()).Unix()
}

func WildFlowSubmissionRetryDeadline() int64 {
	return time.Now().Add(wildFlowSubmissionRetryWindow()).Unix()
}

func ReconcileWildFlowSubmissionLeasesOnce(now int64, limit int) (int, error) {
	return model.ReconcileExpiredWildFlowSubmissionLeases(now, limit)
}

func ReconcileWildFlowSubmissionLease(operationID string, now int64) (bool, error) {
	return model.ReconcileWildFlowSubmissionLease(operationID, now)
}
