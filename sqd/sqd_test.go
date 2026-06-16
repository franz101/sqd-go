package sqd_test

import (
	"testing"

	"github.com/franz101/sqd-go/internal/database"
	"github.com/franz101/sqd-go/internal/ingestion"
	"github.com/franz101/sqd-go/sqd"
)

// Compile-time guarantees that the public facade aliases are the *same* types as
// the underlying internal types. This is the whole point of the facade: a project
// that imports only sqd must still satisfy the in-module ingestion APIs. If any
// alias drifted to a distinct type these assignments would stop compiling.
var (
	_ sqd.Processor       = ingestion.Processor(nil)
	_ ingestion.Processor = sqd.Processor(nil)
	_ *sqd.Store          = (*database.Store)(nil)
	_ *database.Store     = (*sqd.Store)(nil)
	_ sqd.CustomLog       = ingestion.CustomLog{}
	_ ingestion.CustomLog = sqd.CustomLog{}
)

func TestFacadeForwards(t *testing.T) {
	// GetProtoMode forwards to the CLI and returns a bool (value depends on global
	// CLI state; we only exercise the forward).
	_ = sqd.GetProtoMode()

	// RegisterProcessor / RegisterProcessorV2 forward to the registry. An empty
	// name or nil factory is a documented no-op and must not panic.
	sqd.RegisterProcessor("", nil)
	sqd.RegisterProcessorV2("", nil)
	sqd.RegisterProcessor("sqd_facade_test", func() (sqd.Processor, error) { return nil, nil })
	sqd.RegisterProcessorV2("sqd_facade_test_v2", func(bool) (sqd.Processor, error) { return nil, nil })
}
