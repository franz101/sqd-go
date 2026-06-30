package codegen

import (
	"fmt"
	"go/format"

	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/template"
)

func generateCompactionGo(cfg *config.Config, tables []customTableSpec) ([]byte, error) {
	tmplData := struct {
		Tables []customTableSpec
	}{
		Tables: tables,
	}

	src := template.MustExecute("code/compactionGo", tmplData)

	formatted, err := format.Source([]byte(src))
	if err != nil {
		return []byte(src), fmt.Errorf("format compaction source: %w", err)
	}
	return formatted, nil
}
