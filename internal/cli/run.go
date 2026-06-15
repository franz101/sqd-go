package cli

import (
	"context"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"syscall"

	"github.com/franz101/sqd-go/internal/codegen"
	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/ingestion"
)

// runDev loads the project, runs codegen, starts docker compose, then runs the
// ingestion pipeline. On exit it tears down docker compose. Use this for local
// development where ClickHouse is managed by compose.
func runDev(path string, restart, protoMode, noColdCache bool, startBlockStr, endBlockStr, chainIDStr, cpuprofile string, pageSizeStr string, parallelFetch bool) int {
	log.Printf("dev: loading project %s", path)
	project, err := config.LoadProject(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return 1
	}
	applyOverrides(project.Config, protoMode, startBlockStr, endBlockStr, chainIDStr)
	loadEnv(filepath.Join(project.Root, ".env"))
	outPath, err := codegen.GenerateProject(project)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codegen: %v\n", err)
		return 1
	}
	log.Printf("codegen: %s", outPath)

	// docker compose up
	cf := findComposeFile(project.Root)
	if cf != "" {
		log.Printf("docker compose -f %s up -d", cf)
		cmd := exec.Command("docker", "compose", "-f", cf, "up", "-d")
		cmd.Dir = project.Root
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "docker compose up: %v\n", err)
			return 1
		}
		defer func() {
			log.Println("docker compose down")
			dcmd := exec.Command("docker", "compose", "-f", cf, "down", "-v")
			dcmd.Dir = project.Root
			dcmd.Run()
		}()
	}

	return runStartPipelineInternal(project, path, restart, protoMode, noColdCache, outPath, cpuprofile, pageSizeStr, parallelFetch)
}

// runStartPipeline loads the project, runs codegen, then starts ingestion.
// Unlike runDev it does not manage docker compose — the user is responsible for
// running ClickHouse externally.
func runStartPipeline(path string, restart, protoMode, noColdCache bool, startBlockStr, endBlockStr, chainIDStr, cpuprofile string, pageSizeStr string, parallelFetch bool) int {
	project, err := config.LoadProject(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return 1
	}
	applyOverrides(project.Config, protoMode, startBlockStr, endBlockStr, chainIDStr)
	loadEnv(filepath.Join(project.Root, ".env"))
	outPath, err := codegen.GenerateProject(project)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codegen: %v\n", err)
		return 1
	}
	log.Printf("codegen: %s", outPath)

	return runStartPipelineInternal(project, path, restart, protoMode, noColdCache, outPath, cpuprofile, pageSizeStr, parallelFetch)
}

func applyOverrides(cfg *config.Config, protoMode bool, startBlockStr, endBlockStr, chainIDStr string) {
	if protoMode {
		cfg.ProtoMode = &protoMode
	}

	if chainIDStr != "" {
		id, err := chainIDFromName(chainIDStr)
		if err == nil {
			for i := range cfg.Chains {
				cfg.Chains[i].ID = id
			}
		}
	}
	if startBlockStr != "" {
		start, err := strconv.ParseUint(startBlockStr, 10, 64)
		if err == nil {
			for i := range cfg.Chains {
				cfg.Chains[i].StartBlock = start
			}
		}
	}
	if endBlockStr != "" {
		end, err := strconv.ParseUint(endBlockStr, 10, 64)
		if err == nil {
			for i := range cfg.Chains {
				cfg.Chains[i].EndBlock = &end
			}
		}
	}
}

// runStartPipelineInternal is the shared core of runDev and runStartPipeline.
// It configures the ingestion options (ClickHouse connection, page size, cold
// cache), resolves the custom processor, and calls ingestion.Run.
//
// V1 (parsed mode) is still accessible via --no-proto. When proto mode is off,
// the pipeline falls back to the legacy JSON-decoded path with struct-based
// event processing. This is useful for debugging or when proto support has not
// been validated for a new contract.
func runStartPipelineInternal(project *config.Project, path string, restart, protoMode, noColdCache bool, outPath, cpuprofile string, pageSizeStr string, parallelFetch bool) int {
	if protoMode {
		log.Printf("V2 PROTO MODE ENABLED: zero-copy views, proto-only storage")
	}
	SetProtoMode(protoMode)
	SetV3Mode(false)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// SQD_PPROF_ADDR=localhost:6060 exposes net/http/pprof on a live run so
	// profiles can be pulled without stopping the pipeline.
	if addr := os.Getenv("SQD_PPROF_ADDR"); addr != "" {
		go func() {
			log.Printf("pprof listening on http://%s/debug/pprof/", addr)
			if err := http.ListenAndServe(addr, nil); err != nil {
				log.Printf("pprof server: %v", err)
			}
		}()
	}

	if cpuprofile != "" {
		f, err := os.Create(cpuprofile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "create cpu profile: %v\n", err)
			return 1
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			_ = f.Close()
			fmt.Fprintf(os.Stderr, "start cpu profile: %v\n", err)
			return 1
		}
		defer func() {
			pprof.StopCPUProfile()
			if err := f.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "close cpu profile: %v\n", err)
				return
			}
			log.Printf("cpu profile written: %s", cpuprofile)
		}()
	}

	var pageSize uint64 = 0
	if pageSizeStr != "" {
		if val, err := parseUintFlag("--pagesize", pageSizeStr, 0); err == nil {
			pageSize = val
		}
	}

	opts := ingestion.Options{
		ClickHouseHost:     envOrDefault("CLICKHOUSE_HOST", "127.0.0.1"),
		ClickHousePort:     envOrDefaultInt("CLICKHOUSE_NATIVE_PORT", 9000),
		ClickHouseUser:     envOrDefault("CLICKHOUSE_USER", "default"),
		ClickHousePassword: envOrDefault("CLICKHOUSE_PASSWORD", "sqd-clickhouse"),
		ClickHouseDatabase: envOrDefault("CLICKHOUSE_DATABASE", project.Config.Name),
		Restart:            restart,
		GeneratedSQLDir:    filepath.Dir(outPath),
		CursorMode:         true,
		PageSize:           pageSize,
		ColdCache:          resolveColdCache(noColdCache, project.Config.ColdCache),
		ParallelFetch:      parallelFetch,
	}
	if parallelFetch {
		workers, pageBlocks := ingestion.ParallelFetchSettings()
		log.Printf("PARALLEL FETCH ENABLED: finalized backfill via %d concurrent range workers (page %d blocks)", workers, pageBlocks)
	}
	if opts.ColdCache {
		log.Printf("COLD TIER ENABLED: per-miss ClickHouse SELECTs served from local Pebble (off-heap, bounded)")
	}
	processor, err := processorForProject(project.Config.Name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "custom processor: %v\n", err)
		return 1
	}
	opts.Processor = processor
	log.Printf("starting ingestion for %s (pageSize=%d)", project.Config.Name, pageSize)
	if err := ingestion.Run(ctx, project.Config, opts); err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "ingestion error: %v\n", err)
		return 1
	}

	return 0
}

func runStop() int {
	cf := findComposeFile(".")
	if cf == "" {
		fmt.Fprintln(os.Stderr, "no compose.yml found in current directory")
		return 1
	}
	log.Printf("stopping environment in %s", filepath.Dir(cf))
	cmd := exec.Command("docker", "compose", "-f", cf, "down", "-v")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "docker compose down: %v\n", err)
		return 1
	}
	return 0
}

func runInitContractImport(p *parsedArgs) int {
	if p.initABI == "" {
		fmt.Fprintln(os.Stderr, "usage: sqd-go init contract-import local --abi <file> --name <name> [--address <addr>]")
		return 2
	}
	if p.initSource == "" {
		p.initSource = "local"
	}
	if p.initSource != "local" {
		fmt.Fprintf(os.Stderr, "unsupported contract-import source %q (only \"local\" is supported)\n", p.initSource)
		return 2
	}
	name := p.initName
	if name == "" {
		name = "Contract"
	}
	chainID, err := chainIDFromName(p.initChainID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid blockchain: %v\n", err)
		return 2
	}
	startBlock, err := parseUintFlag("--start-block", p.initStartBlk, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid start block: %v\n", err)
		return 2
	}
	projectDir := p.project
	if projectDir == "" {
		projectDir = defaultProjectDirFromName(name)
	}

	abiData, err := os.ReadFile(p.initABI)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read abi: %v\n", err)
		return 1
	}
	events, err := extractEventsFromABI(abiData)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse abi: %v\n", err)
		return 1
	}
	if len(events) == 0 {
		fmt.Fprintln(os.Stderr, "no events found in ABI")
		return 1
	}
	eventConfigs := make([]config.EventConfig, len(events))
	for i, event := range events {
		eventConfigs[i] = config.EventConfig{Event: event}
	}

	var address config.Address
	if p.initAddress != "" {
		address = config.Address{p.initAddress}
	}
	cfg := config.Config{
		Name:      name,
		Ecosystem: stringPtr("evm"),
		Chains: []config.Chain{{
			ID:         chainID,
			StartBlock: startBlock,
			Contracts: []config.ChainContractConfig{{
				Name:    name,
				Address: address,
				Events:  eventConfigs,
			}},
		}},
	}
	configPath, err := writeInitProjectFiles(projectDir, &cfg, p.initABI)
	if err != nil {
		fmt.Fprintf(os.Stderr, "write project: %v\n", err)
		return 1
	}
	fmt.Printf("initialized %s (%d events)\n", configPath, len(events))
	return 0
}

// resolveColdCache determines whether the cold cache should be enabled.
// On by default. Disabled by --no-cold-cache or config cold_cache: false.
func resolveColdCache(flagOff bool, configVal *bool) bool {
	if flagOff {
		return false
	}
	if configVal != nil {
		return *configVal
	}
	return true
}

func findComposeFile(root string) string {
	for _, name := range []string{"compose.yml", "compose.yaml", "docker-compose.yml", "docker-compose.yaml"} {
		path := filepath.Join(root, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}
