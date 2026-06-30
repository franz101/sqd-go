package codegen

import (
	"fmt"
	"go/format"

	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/template"
)

type initScaffoldData struct {
	PackageName     string
	GeneratedImport string
	ERC20           bool
	EventGoType     string
	FromField       string
	ToField         string
	ValueField      string
}

// RenderStateScaffold renders a project-level custom schema and processor from
// config-derived event metadata. ERC-20 Transfer events get a useful positions
// example; every other ABI gets a compiling stub for its first generated event.
func RenderStateScaffold(cfg *config.Config, packageName, generatedImport string) ([]byte, []byte, error) {
	events, err := buildEventSpecs(cfg)
	if err != nil {
		return nil, nil, err
	}
	if len(events) == 0 {
		return nil, nil, fmt.Errorf("cannot scaffold state without a configured event")
	}

	selected := events[0]
	erc20 := false
	for _, event := range events {
		if isERC20Transfer(event) {
			selected = event
			erc20 = true
			break
		}
	}

	data := initScaffoldData{
		PackageName:     packageName,
		GeneratedImport: generatedImport,
		ERC20:           erc20,
		EventGoType:     selected.GoTypeName,
	}
	if erc20 {
		data.FromField = selected.Args[0].GoFieldName
		data.ToField = selected.Args[1].GoFieldName
		data.ValueField = selected.Args[2].GoFieldName
	}

	schema, err := format.Source([]byte(template.MustExecute("code/initCustomSchema", data)))
	if err != nil {
		return nil, nil, fmt.Errorf("format custom schema scaffold: %w", err)
	}
	processor, err := format.Source([]byte(template.MustExecute("code/initCustomProcessor", data)))
	if err != nil {
		return nil, nil, fmt.Errorf("format custom processor scaffold: %w", err)
	}
	return schema, processor, nil
}

// isERC20Transfer reports whether an event is an ERC-20 Transfer(address,address,uint256).
// Used by the init scaffolder to provide a useful Transfer example.
func isERC20Transfer(ev eventSpec) bool {
	return ev.CanonicalSig == "Transfer(address,address,uint256)" &&
		len(ev.Args) == 3 &&
		ev.Args[0].SolidityType == "address" &&
		ev.Args[1].SolidityType == "address" &&
		ev.Args[2].SolidityType == "uint256"
}
