package cli

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/franz101/sqd-go/internal/codegen"
	"github.com/franz101/sqd-go/internal/config"
)

type parsedArgs struct {
	command       string
	project       string
	restart       bool
	noColdCache   bool
	parallelFetch bool
	prefetch      bool
	noReplay      bool
	state         bool
	protoMode     bool
	// reindexFrom holds the --reindex-from value: a block number above which all
	// blocks are deleted (lightweight DELETE) before ingestion resumes, so a block
	// range can be re-derived without a full restart. Empty string = disabled.
	reindexFrom  string
	initSource   string
	initABI      string
	initName     string
	initAddress  string
	initChainID  string
	initStartBlk string
	initEndBlk   string
	cpuprofile   string
	fgprofile    string
	pageSize     string
}

func parseArgs(args []string) (*parsedArgs, error) {
	p := &parsedArgs{
		protoMode: true,
	}
	positional := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		a := args[i]
		// Handle --flag=value syntax
		if strings.Contains(a, "=") {
			parts := strings.SplitN(a, "=", 2)
			if len(parts) == 2 {
				a = parts[0]
				args = append(args[:i], append([]string{a, parts[1]}, args[i+1:]...)...)
				// Re-process the flag with its value as a separate arg
				i--
				continue
			}
		}
		switch a {
		case "-r", "--restart":
			p.restart = true
		case "--no-resume":
			p.restart = true
		case "-a", "--abi":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--abi requires a value")
			}
			p.initABI = args[i]
		case "-n", "--name":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--name requires a value")
			}
			p.initName = args[i]
		case "--address", "--addr":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--address requires a value")
			}
			p.initAddress = args[i]
		case "-b", "--blockchain":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--blockchain requires a value")
			}
			p.initChainID = args[i]
		case "-s", "--start-block":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--start-block requires a value")
			}
			p.initStartBlk = args[i]
		case "-e", "--end-block":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--end-block requires a value")
			}
			p.initEndBlk = args[i]
		case "--cpuprofile":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--cpuprofile requires a value")
			}
			p.cpuprofile = args[i]
		case "--fgprofile":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--fgprofile requires a value")
			}
			p.fgprofile = args[i]
		case "--no-proto":
			p.protoMode = false
		case "--no-cold-cache":
			p.noColdCache = true
		case "--no-replay":
			p.noReplay = true
		case "--parallel-fetch":
			p.parallelFetch = true
		case "--prefetch":
			p.prefetch = true
		case "--state":
			p.state = true
		case "-p", "--pagesize":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--pagesize requires a value")
			}
			p.pageSize = args[i]
		case "--reindex-from":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--reindex-from requires a value")
			}
			p.reindexFrom = args[i]
		default:
			if !strings.HasPrefix(a, "-") {
				positional = append(positional, a)
			} else {
				// Error on unknown dash tokens instead of silently dropping them
				return nil, fmt.Errorf("unknown flag: %s", a)
			}
		}
	}

	if len(positional) > 0 {
		p.command = positional[0]
	}
	if p.command == "init" {
		if len(positional) > 1 {
			p.command = "init:" + positional[1]
		}
		if len(positional) > 2 {
			p.initSource = positional[2]
		}
		if len(positional) > 3 {
			p.project = positional[3]
		}
	} else if p.command != "stop" && p.command != "help" && len(positional) > 1 {
		p.project = positional[1]
	}

	return p, nil
}

func Run(args []string) int {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	loadEnv(".env") // cwd
	if exe, err := os.Executable(); err == nil {
		loadEnv(filepath.Join(filepath.Dir(exe), ".env")) // binary dir
	}

	p, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse args: %v\n", err)
		return 2
	}

	if p.command == "" {
		return runInteractiveInit()
	}

	switch p.command {
	case "codegen":
		if p.project == "" {
			fmt.Fprintln(os.Stderr, "usage: sqd-go codegen <project-dir|config.yaml|config.yml>")
			return 2
		}
		project, err := config.LoadProject(p.project)
		if err != nil {
			fmt.Fprintf(os.Stderr, "load config: %v\n", err)
			return 1
		}
		outPath, err := codegen.GenerateProject(project)
		if err != nil {
			fmt.Fprintf(os.Stderr, "codegen: %v\n", err)
			return 1
		}
		fmt.Printf("generated %s\n", filepath.Dir(outPath))
		fmt.Printf("generated %s\n", filepath.Join(project.Root, "generated", "events.go"))
		return 0

	case "start":
		if p.project == "" {
			fmt.Fprintln(os.Stderr, "usage: sqd-go start <project-dir|config.yaml|config.yml> [--restart] [--state] [--start-block <n>] [--end-block <n>] [--blockchain <id|name>]")
			return 2
		}
		// --state regenerates the project and re-execs a binary that has the
		// project's processor compiled in, so custom state + the PK cold cache
		// actually run. The re-execed child carries SQD_STATE_CHILD and falls
		// through to the normal pipeline below.
		if p.state && os.Getenv(stateChildEnv) == "" {
			return runStateRebuild(args, p.project)
		}
		return runStartPipeline(p.project, p.restart, p.protoMode, p.noColdCache, p.initStartBlk, p.initEndBlk, p.initChainID, p.cpuprofile, p.fgprofile, p.pageSize, p.parallelFetch, p.reindexFrom, p.prefetch, p.noReplay)

	case "dev":
		if p.project == "" {
			fmt.Fprintln(os.Stderr, "usage: sqd-go dev <project-dir|config.yaml|config.yml> [--restart]")
			return 2
		}
		return runDev(p.project, p.restart, p.protoMode, p.noColdCache, p.initStartBlk, p.initEndBlk, p.initChainID, p.cpuprofile, p.fgprofile, p.pageSize, p.parallelFetch, p.reindexFrom, p.prefetch, p.noReplay)

	case "stop":
		return runStop()

	case "init":
		return runInteractiveInit()

	case "init:contract-import":
		return runInitContractImport(p)

	case "init:template":
		return runInitTemplate(p)

	case "-h", "--help", "help":
		fmt.Print(usage())
		return 0

	default:
		return runStartPipeline(p.command, p.restart, p.protoMode, p.noColdCache, p.initStartBlk, p.initEndBlk, p.initChainID, p.cpuprofile, p.fgprofile, p.pageSize, p.parallelFetch, p.reindexFrom, p.prefetch, p.noReplay)
	}
}

func loadEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}

func envOrDefault(key, defaultVal string) string {
	// Honor an explicitly-set value even when it is empty (e.g.
	// CLICKHOUSE_PASSWORD='' means "no password", not "use the default").
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return defaultVal
}

func envOrDefaultInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return defaultVal
		}
		return n
	}
	return defaultVal
}

func envOrDefaultBool(key string, defaultVal bool) bool {
	if v := strings.TrimSpace(strings.ToLower(os.Getenv(key))); v != "" {
		switch v {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		default:
			return defaultVal
		}
	}
	return defaultVal
}

func stringPtr(v string) *string {
	return &v
}

func ptrStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func chainIDFromName(name string) (uint64, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "ethereum", "mainnet", "1":
		return 1, nil
	case "polygon", "matic", "137":
		return 137, nil
	case "":
		return 1, nil
	default:
		return parseUintFlag("--blockchain", name, 0)
	}
}

func parseUintFlag(flag, value string, defaultValue uint64) (uint64, error) {
	if strings.TrimSpace(value) == "" {
		return defaultValue, nil
	}
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an unsigned integer: %w", flag, err)
	}
	return n, nil
}

func usage() string {
	return strings.TrimSpace(`
sqd-go — EVM log indexer

Usage:
  sqd-go [command] [arguments]

Commands:
  init                 Interactive EVM project setup
  codegen <path>       Validate config and generate manifest (path can be dir or config.yaml/yml)
  start   <path>       Run codegen, then start ingestion
  dev     <path>       codegen + docker compose up + start (dev mode)
  stop                 Stop local env (docker compose down + drop DB)
  init    contract-import local [path] --abi <file> --name <name> [--address <addr>]
                       Scaffold a new project from an ABI file
  init    template [erc20] [path]
                       Scaffold an ERC20 template project
  help                 Show this help

Flags:
  --restart             (dev/start) Drop DB and re-index from scratch
  --abi, -a             (init) Path to ABI JSON file
  --name, -n            (init) Contract name
  --address, --addr     (init) Contract address (hex)
  --blockchain, -b      (init/start) Chain ID or name (default: 1)
  --start-block, -s     (init/start) Start block (default: 0)
  --end-block, -e       (start) End block (0 for infinite)
  --pagesize, -p        (start/dev) Fixed page size range to fetch (default: 0 for dynamic)
  --no-proto            (start/dev) Use V1 legacy parsed mode instead of proto (struct-based event
                       processing with JSON decode; useful for debugging or unvalidated contracts)
  --no-cold-cache       (start/dev) Disable the Pebble cold tier (on by default)
  --no-replay           (start/dev) Disable fork recovery. The replay buffer is reduced to a small
                       smoothing pipe; fork errors are fatal. Use for backfill-only or benchmarking
                       runs where reorgs cannot occur or do not matter.
  --state               (start) Regenerate the project and re-exec a binary with the project's
                       processor compiled in, so custom state + the PK-keyed cold tier actually run
                       in one command (no manual rebuild). Needs the Go toolchain; the project must
                       live inside the sqd-go module and have a custom_schema.go/custom_processor.go.
  --parallel-fetch      (start/dev) Fetch the finalized backfill range with concurrent range workers,
                       paced by a shared rate limiter (the portal caps ~5 req/s). Skips empty blocks
                       (includeAllBlocks=false) unless the project stores raw blocks/logs. Tune via
                       SQD_PARALLEL_FETCHERS (default 6), SQD_PARALLEL_PAGE (default 10000),
                       SQD_PARALLEL_RPS (default 5).
	  --reindex-from        (start/dev) Delete all blocks above the specified block using lightweight
	                       DELETE and resume indexing from that block. Data at or below this block is
	                       preserved. Useful for reindexing after a contract fix.
  --prefetch            (start/dev) Two-pass batch prefetch: dispatch each block once to collect its
                       hot-state read-set, resolve the misses in one ClickHouse SELECT per entity, then
                       process for real against a warm cache. Collapses the lazy path's
                       one-SELECT-per-missing-key into one per entity per block. Off by default; most
                       useful in resume mode against a populated ClickHouse.

Examples:
  sqd-go
  sqd-go init
  sqd-go codegen examples/uniswap
  sqd-go start examples/uniswap
  sqd-go start examples/uniswap --blockchain polygon --start-block 80000000 --restart
  sqd-go start examples/uniswap --end-block 20000835 --parallel-fetch --restart
  sqd-go start examples/uniswap --state --restart   # run custom state + PK cold cache in one command
  sqd-go start examples/uniswap --restart
  sqd-go dev examples/uniswap --restart
  sqd-go stop
  sqd-go init contract-import local --abi erc20.json --name USDC --address 0xA0...
`)
}
