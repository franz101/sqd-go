package cli

import (
	"fmt"
	"strings"
	"sync"

	"github.com/franz101/sqd-go/internal/ingestion"
)

type processorFactory func() (ingestion.Processor, error)
type processorFactoryV2 func(protoMode bool) (ingestion.Processor, error)

var processorFactories sync.Map
var protoMode bool // V2: global proto mode flag
var v3Mode bool    // V3: global v3 mode flag

// SetProtoMode sets the global proto mode flag (called from CLI)
func SetProtoMode(enabled bool) {
	fmt.Printf("[REGISTRY DEBUG] SetProtoMode called with enabled=%v\n", enabled)
	protoMode = enabled
	fmt.Printf("[REGISTRY DEBUG] protoMode is now set to %v\n", protoMode)
}

// GetProtoMode returns the current proto mode flag
func GetProtoMode() bool {
	return protoMode
}

// SetV3Mode sets the global v3 mode flag (called from CLI)
func SetV3Mode(enabled bool) {
	fmt.Printf("[REGISTRY DEBUG] SetV3Mode called with enabled=%v\n", enabled)
	v3Mode = enabled
}

// GetV3Mode returns the current v3 mode flag
func GetV3Mode() bool {
	return v3Mode
}

func RegisterProcessor(projectName string, factory func() (ingestion.Processor, error)) {
	name := strings.TrimSpace(strings.ToLower(projectName))
	if name == "" || factory == nil {
		return
	}
	processorFactories.Store(name, processorFactory(factory))
}

// RegisterProcessorV2 registers a processor factory that takes protoMode as parameter
func RegisterProcessorV2(projectName string, factory func(protoMode bool) (ingestion.Processor, error)) {
	name := strings.TrimSpace(strings.ToLower(projectName))
	if name == "" || factory == nil {
		return
	}
	processorFactories.Store(name, processorFactoryV2(factory))
}

func processorForProject(projectName string) (ingestion.Processor, error) {
	name := strings.TrimSpace(strings.ToLower(projectName))
	if name == "" {
		return nil, nil
	}
	value, ok := processorFactories.Load(name)
	if !ok {
		return nil, nil
	}

	// DEBUG: Log registry state
	fmt.Printf("[REGISTRY DEBUG] protoMode=%v, looking for factory for %s\n", protoMode, name)

	// Try V2 factory first (with proto mode support)
	if factoryV2, ok := value.(processorFactoryV2); ok {
		fmt.Printf("[REGISTRY DEBUG] Using V2 factory with protoMode=%v\n", protoMode)
		return factoryV2(protoMode)
	}

	// Fall back to V1 factory (no proto mode support)
	if factory, ok := value.(processorFactory); ok {
		fmt.Printf("[REGISTRY DEBUG] Using V1 factory (no proto mode support)\n")
		return factory()
	}

	return nil, nil
}
