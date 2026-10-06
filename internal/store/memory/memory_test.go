// Copyright 2026 Elliott Wave LLC. All rights reserved.

package memory_test

import (
	"testing"

	"github.com/moqom-cloud/motom/internal/store"
	"github.com/moqom-cloud/motom/internal/store/memory"
	"github.com/moqom-cloud/motom/internal/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(*testing.T) store.Store { return memory.New() })
}
