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

// SetProtoMode sets the global proto mode flag (called from CLI).
func SetProtoMode(enabled bool) {
	protoMode = enabled
}

// GetProtoMode returns the current proto mode flag.
func GetProtoMode() bool {
	return protoMode
}

// SetV3Mode sets the global v3 mode flag (called from CLI).
func SetV3Mode(enabled bool) {
	v3Mode = enabled
}

// GetV3Mode returns the current v3 mode flag
func GetV3Mode() bool {
	return v3Mode
}

// RegisterProcessor registers a v1 processor factory for the given project name.
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

// registeredNames returns a sorted slice of all registered processor names.
// Useful for error messages when a lookup fails.
func registeredNames() []string {
	var names []string
	processorFactories.Range(func(key, value interface{}) bool {
		if name, ok := key.(string); ok {
			names = append(names, name)
		}
		return true
	})
	return names
}

func processorForProject(projectName string) (ingestion.Processor, error) {
	name := strings.TrimSpace(strings.ToLower(projectName))
	if name == "" {
		return nil, nil
	}
	value, ok := processorFactories.Load(name)
	if !ok {
		// Check if any processors are registered at all - if so, this is a name mismatch
		registered := registeredNames()
		if len(registered) > 0 {
			return nil, fmt.Errorf("config name %q matches no registered processor; registered: [%s]. "+
				"Register under generated.ProjectName in custom_processor.go init()",
				projectName, strings.Join(registered, ", "))
		}
		return nil, nil
	}

	if factoryV2, ok := value.(processorFactoryV2); ok {
		return factoryV2(protoMode)
	}
	if factory, ok := value.(processorFactory); ok {
		return factory()
	}

	return nil, nil
}
