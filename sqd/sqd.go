// Package sqd is the public API surface that sqd-go projects compile against.
//
// A project's generated package and its hand-written custom_processor.go import
// this package instead of the module's internal/ packages. That indirection is
// what lets a project be built as its own standalone Go module — e.g. a notebook
// with only custom_schema.go + custom_processor.go and a scaffolded go.mod that
// requires github.com/franz101/sqd-go — without having to live inside a sqd-go
// checkout. Go forbids importing another module's internal/ packages, so the
// generated code could never compile out-of-module while it referenced
// internal/cli, internal/ingestion or internal/database directly.
//
// The decode helpers and cold tier are exposed as their own public packages
// (github.com/franz101/sqd-go/abiunpack and .../coldcache); this package covers
// the registration/runtime glue.
package sqd

import (
	"github.com/franz101/sqd-go/internal/cli"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/franz101/sqd-go/internal/ingestion"
)

// Processor is the ingestion processor a project's generated package implements,
// and the value a RegisterProcessor factory returns.
type Processor = ingestion.Processor

// CustomLog is one decoded event log passed to a v1 processor's Process method.
type CustomLog = ingestion.CustomLog

// Store is the ClickHouse-backed store handed to generated Process/Flush.
type Store = database.Store

// RegisterProcessor registers a v1 processor factory for a project by name.
// Call it from your project package's init():
//
//	func init() {
//	    generated.CustomProcessFn = Process
//	    sqd.RegisterProcessor(generated.ProjectName, func() (sqd.Processor, error) {
//	        return generated.NewProcessor(sqd.GetProtoMode())
//	    })
//	}
func RegisterProcessor(projectName string, factory func() (Processor, error)) {
	cli.RegisterProcessor(projectName, factory)
}

// RegisterProcessorV2 registers a processor factory that receives the proto mode.
func RegisterProcessorV2(projectName string, factory func(protoMode bool) (Processor, error)) {
	cli.RegisterProcessorV2(projectName, factory)
}

// GetProtoMode reports whether the CLI was started in proto mode.
func GetProtoMode() bool { return cli.GetProtoMode() }

// Run is the CLI entry point. A --state runner main is just:
//
//	func main() { os.Exit(sqd.Run(os.Args[1:])) }
func Run(args []string) int { return cli.Run(args) }
