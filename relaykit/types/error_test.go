package types

import (
	"fmt"
	"testing"
)

func TestNewOpenAIErrorPreservesProviderUnsentMarkerAcrossNewAPIError(t *testing.T) {
	inner := NewError(fmt.Errorf("invalid header override"), ErrorCodeChannelHeaderOverrideInvalid)
	wrapped := fmt.Errorf("%w: outbound request setup: %w", ErrProviderNotDispatched, inner)

	got := NewOpenAIError(wrapped, ErrorCodeDoRequestFailed, 500)
	if !got.IsProviderUnsent() {
		t.Fatal("provider-unsent marker was lost while preserving the nested NewAPIError")
	}
}
