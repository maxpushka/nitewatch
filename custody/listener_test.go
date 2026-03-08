package custody

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWaitForBackOffTimeout_ExceedsLimit(t *testing.T) {
	ctx := context.Background()

	// Zero backoff returns immediately.
	assert.True(t, waitForBackOffTimeout(ctx, 0, "test"))

	// Over the max limit should return false immediately.
	assert.False(t, waitForBackOffTimeout(ctx, maxBackOffCount+1, "test"))
}

func TestWaitForBackOffTimeout_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Should return false when context is cancelled, even with backoff > 0.
	assert.False(t, waitForBackOffTimeout(ctx, 1, "test"))
}

func TestErrBackoffLimitReached(t *testing.T) {
	assert.NotNil(t, ErrBackoffLimitReached)
	assert.Contains(t, ErrBackoffLimitReached.Error(), "backoff limit reached")
}
