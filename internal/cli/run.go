package cli

import (
	"context"
	"fmt"
	"log"
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
func runDev(path string, restart, protoMode, noColdCache bool, startBlockStr, endBlockStr, chainIDStr, cpuprofile string, pageSizeStr string, parallelFetch bool, reindexFromStr string, prefetch bool) int {
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

	return runStartPipelineInternal(project, path, restart, protoMode, noColdCache, outPath, cpuprofile, pageSizeStr, parallelFetch, reindexFromStr, prefetch)
}

// runStartPipeline loads the project, runs codegen, then starts ingestion.
// Unlike runDev it does not manage docker compose — the user is responsible for
// running ClickHouse externally.
func runStartPipeline(path string, restart, protoMode, noColdCache bool, startBlockStr, endBlockStr, chainIDStr, cpuprofile string, pageSizeStr string, parallelFetch bool, reindexFromStr string, prefetch bool) int {
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

	return runStartPipelineInternal(project, path, restart, protoMode, noColdCache, outPath, cpuprofile, pageSizeStr, parallelFetch, reindexFromStr, prefetch)
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
				if end == 0 {
					cfg.Chains[i].EndBlock = nil
				} else {
					cfg.Chains[i].EndBlock = &end
				}
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
func runStartPipelineInternal(project *config.Project, path string, restart, protoMode, noColdCache bool, outPath, cpuprofile string, pageSizeStr string, parallelFetch bool, reindexFromStr string, prefetch bool) int {
	if protoMode {
		log.Printf("V2 PROTO MODE ENABLED: zero-copy views, proto-only storage")
	}
	SetProtoMode(protoMode)
	SetV3Mode(false)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

	var reindexFrom uint64 = 0
	if reindexFromStr != "" {
		if val, err := parseUintFlag("--reindex-from", reindexFromStr, 0); err == nil {
			reindexFrom = val
			log.Printf("REINDEX FROM BLOCK %d: will delete all blocks > %d using lightweight DELETE", val, val)
		} else {
			fmt.Fprintf(os.Stderr, "invalid --reindex-from value: %v\n", err)
			return 1
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
		ReindexFrom:        reindexFrom,
		Prefetch:           prefetch,
	}
	if prefetch {
		log.Printf("PREFETCH ENABLED: two-pass batch read-set prefetch (one ClickHouse SELECT per entity per block instead of one per missing key)")
	}
	if parallelFetch {
		workers, pageBlocks, rps := ingestion.ParallelFetchSettings()
		log.Printf("PARALLEL FETCH ENABLED: finalized backfill via %d concurrent range workers (page %d blocks, ~%.0f req/s shared)", workers, pageBlocks, rps)
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
	// A nil processor means no project package registered one in this binary — the
	// usual cause is running a prebuilt `sqd-go` (go install …@latest) whose build
	// tree did not include this project, so the generated package's init() →
	// RegisterProcessor was never compiled in. Without it the run still indexes raw
	// events, but custom state and the cold tier cannot engage (the "cold cache
	// requested but processor does not implement ColdCacheProcessor" line downstream
	// is this same condition). Say so up front with the fix, instead of leaving the
	// operator to decode a cryptic capability log.
	if processor == nil && (opts.ColdCache || hasStatefulSchema(project)) {
		log.Printf("NOTE: no compiled processor registered for %q — custom state and the cold tier are DISABLED for this run.", project.Config.Name)
		log.Printf("      The prebuilt binary does not contain this project's generated package. Build sqd-go from a checkout that includes %q (so its custom_processor.go init() is compiled in), then re-run, e.g.:", project.Root)
		log.Printf("        go run . start %s        # or: go build -o sqd-go . && ./sqd-go start %s", path, path)
	}
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
		fmt.Fprintf(os.Stderr, "no events found in %s: the ABI has no \"type\":\"event\" entries; router/library contracts emit no events — use the ABI of the contract that emits them (e.g. for Uniswap V2, the Factory or a Pair, not the Router)\n", p.initABI)
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

// resolveColdCache determines whether the cold cache should be enabled. The
// cold cache is ON by default. It is disabled when --no-cold-cache is passed or
// when the config explicitly sets cold_cache: false.
func resolveColdCache(flagOff bool, configVal *bool) bool {
	if flagOff {
		return false
	}
	if configVal != nil {
		return *configVal
	}
	return true
}

// hasStatefulSchema reports whether the project defines derived state — either a
// `state:` block in config or a custom_schema.go in the project root. Such a
// project needs its compiled processor to do anything beyond raw event indexing
// (and to back the cold tier), so a nil processor is worth flagging loudly.
func hasStatefulSchema(project *config.Project) bool {
	if project == nil {
		return false
	}
	if project.Config != nil && len(project.Config.State) > 0 {
		return true
	}
	if _, err := os.Stat(filepath.Join(project.Root, "custom_schema.go")); err == nil {
		return true
	}
	return false
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
