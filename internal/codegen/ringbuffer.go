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

	protoRingCode, err := generateProtoRingBufferGo(tmplData)
	if err != nil {
		return nil, fmt.Errorf("proto ring buffer: %w", err)
	}

	// Concatenate V1 + V2 and format the whole file once. Formatting the two
	// halves separately and then joining them left a stray double blank line at
	// the seam, so the emitted ringbuffer.go was not gofmt-stable. format.Source
	// on the combined source collapses the seam to a single blank line.
	combined := fmt.Sprintf("%s\n\n%s", src, string(protoRingCode))
	formatted, err := format.Source([]byte(combined))
	if err != nil {
		return []byte(combined), fmt.Errorf("format source: %w", err)
	}
	return formatted, nil
}

// generateProtoRingBufferGo generates the V2 proto ring buffer body. The caller
// concatenates it after the V1 ring buffer and formats the combined file, so the
// returned bytes need not be independently gofmt-clean.
func generateProtoRingBufferGo(tmplData struct {
	Events []eventSpec
}) ([]byte, error) {
	src := template.MustExecute("code/protoRingBufferGo", tmplData)
	return []byte(src), nil
}
