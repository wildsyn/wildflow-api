package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/internal/inferenceclient"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/bytedance/gopkg/util/gopool"
)

const (
	wildFlowBillingReconcileInterval = 15 * time.Second
	wildFlowBillingReconcileBatch    = 100
)

var (
	wildFlowBillingReconcilerOnce    sync.Once
	wildFlowBillingReconcilerRunning atomic.Bool
)

func StartWildFlowBillingReconciler() {
	wildFlowBillingReconcilerOnce.Do(func() {
		if !common.IsMasterNode {
			return
		}
		client, err := newWildFlowBillingInferenceClient()
		if err != nil {
			logger.LogWarn(context.Background(), "WildFlow billing reconciler disabled: "+err.Error())
			return
		}
		interval := wildFlowBillingReconcileInterval
		if seconds := common.GetEnvOrDefault("WILDFLOW_BILLING_RECONCILE_SECONDS", int(interval/time.Second)); seconds >= 5 && seconds <= 300 {
			interval = time.Duration(seconds) * time.Second
		}
		gopool.Go(func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			runWildFlowBillingReconciler(client)
			for range ticker.C {
				runWildFlowBillingReconciler(client)
			}
		})
	})
}

func runWildFlowBillingReconciler(client *inferenceclient.Client) {
	if !wildFlowBillingReconcilerRunning.CompareAndSwap(false, true) {
		return
	}
	defer wildFlowBillingReconcilerRunning.Store(false)
	if _, err := ReconcileWildFlowBillingOnce(context.Background(), client, wildFlowBillingReconcileBatch); err != nil {
		logger.LogWarn(context.Background(), "WildFlow billing reconciliation failed: "+err.Error())
	}
}

func ReconcileWildFlowBillingOnce(ctx context.Context, client *inferenceclient.Client, limit int) (int, error) {
	var reconciliationErrors []error
	if _, err := ReconcileWildFlowSubmissionLeasesOnce(time.Now().Unix(), limit); err != nil {
		reconciliationErrors = append(reconciliationErrors, fmt.Errorf("submission leases: %w", err))
	}
	if _, err := model.ReconcileWildFlowCanonicalBillingAudits(limit); err != nil {
		reconciliationErrors = append(reconciliationErrors, fmt.Errorf("canonical billing audits: %w", err))
	}
	if _, err := model.ReconcileWildFlowBillingLogProjections(limit); err != nil {
		reconciliationErrors = append(reconciliationErrors, fmt.Errorf("billing log projections: %w", err))
	}
	if client == nil {
		return 0, errors.Join(append(reconciliationErrors, fmt.Errorf("nil WildFlow inference client"))...)
	}
	operations, err := model.ListWildFlowOperationsForBillingReconciliation(limit)
	if err != nil {
		return 0, err
	}
	processed := 0
	for _, operation := range operations {
		job, getErr := client.GetJob(ctx, operation.JobID, "user:"+strconv.Itoa(operation.UserID))
		if getErr != nil {
			reconciliationErrors = append(reconciliationErrors, fmt.Errorf("operation %s: %w", operation.OperationID, getErr))
			continue
		}
		processed++
		state := job.State
		errorCode := ""
		switch {
		case state == "succeeded":
			artifactErr := ValidateWildFlowCompletedArtifacts(operation, job.Artifacts)
			switch {
			case errors.Is(artifactErr, ErrWildFlowMissingArtifact):
				state = "recovery_required"
				errorCode = "missing_artifact"
			case errors.Is(artifactErr, ErrWildFlowInvalidArtifact):
				state = "recovery_required"
				errorCode = "invalid_artifact"
			case artifactErr != nil:
				reconciliationErrors = append(reconciliationErrors, fmt.Errorf("operation %s artifact validation: %w", operation.OperationID, artifactErr))
				continue
			}
		case state == "recovery_required":
			errorCode = "recovery_required"
		case state == "failed" || strings.TrimSpace(job.LastError) != "":
			errorCode = "execution_failed"
		}
		if updateErr := model.UpdateWildFlowOperationExecution(operation.OperationID, job.ID, state, errorCode); updateErr != nil {
			reconciliationErrors = append(reconciliationErrors, fmt.Errorf("operation %s persistence: %w", operation.OperationID, updateErr))
			continue
		}
		operation.JobID = job.ID
		operation.State = state
		operation.LastErrorCode = errorCode
		if finalizeErr := FinalizeWildFlowOperationBilling(ctx, operation, job.Artifacts); finalizeErr != nil {
			if errors.Is(finalizeErr, model.ErrWildFlowBillingStateConflict) {
				if recoveryErr := model.UpdateWildFlowOperationExecution(
					operation.OperationID,
					operation.JobID,
					"recovery_required",
					"billing_state_conflict",
				); recoveryErr != nil {
					reconciliationErrors = append(reconciliationErrors, fmt.Errorf("operation %s recovery persistence: %w", operation.OperationID, recoveryErr))
				}
				continue
			}
			reconciliationErrors = append(reconciliationErrors, fmt.Errorf("operation %s billing: %w", operation.OperationID, finalizeErr))
		}
	}
	return processed, errors.Join(reconciliationErrors...)
}

func newWildFlowBillingInferenceClient() (*inferenceclient.Client, error) {
	return inferenceclient.New(inferenceclient.Config{
		BaseURL:           strings.TrimSpace(os.Getenv("WILDFLOW_INFERENCE_URL")),
		Token:             strings.TrimSpace(os.Getenv("WILDFLOW_INTERNAL_TOKEN")),
		Timeout:           30 * time.Second,
		AllowInternalHTTP: common.GetEnvOrDefaultBool("WILDFLOW_INFERENCE_ALLOW_INTERNAL_HTTP", false),
	})
}
