module github.com/unitoftime/ecsbenchmark

go 1.25.0

require (
	github.com/ClickHouse/ch-go v0.72.0
	github.com/franz101/sqd-go v0.0.0-00010101000000-000000000000
	github.com/shopspring/decimal v1.4.0
	github.com/unitoftime/ecs v0.0.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-faster/city v1.0.1 // indirect
	github.com/go-faster/errors v0.7.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/holiman/uint256 v1.3.2 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/pierrec/lz4/v4 v4.1.26 // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/unitoftime/cod v0.0.0-20250419234656-6f109c444441 // indirect
	go.opentelemetry.io/otel v1.43.0 // indirect
	go.opentelemetry.io/otel/trace v1.43.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
)

replace github.com/unitoftime/ecs => ../ecs

replace github.com/franz101/sqd-go => ../..
