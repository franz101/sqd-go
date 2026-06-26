package codegen

import (
	"fmt"
	"go/format"

	"github.com/franz101/sqd-go/internal/template"
)

func generateRingBufferGo(events []eventSpec) ([]byte, error) {
	tmplData := struct {
		Events []eventSpec
	}{
		Events: events,
	}

	src := template.MustExecute("code/ringbufferGo", tmplData)

	formatted, err := format.Source([]byte(src))
	if err != nil {
		return []byte(src), fmt.Errorf("format source: %w", err)
	}

	protoRingCode, err := generateProtoRingBufferGo(tmplData)
	if err != nil {
		return formatted, fmt.Errorf("proto ring buffer: %w", err)
	}

	result := fmt.Sprintf("%s\n\n%s", string(formatted), string(protoRingCode))
	return []byte(result), nil
}

// generateProtoRingBufferGo generates the V2 proto ring buffer.
//
// Keep this formatted separately from ringbufferGo to preserve the old output
// shape exactly: V1 is formatted, V2 is formatted, then both are concatenated.
func generateProtoRingBufferGo(tmplData struct {
	Events []eventSpec
}) ([]byte, error) {
	src := template.MustExecute("code/protoRingBufferGo", tmplData)

	formatted, err := format.Source([]byte(src))
	if err != nil {
		return []byte(src), fmt.Errorf("format proto ring buffer: %w", err)
	}

	return formatted, nil
}
