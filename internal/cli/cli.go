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
	command      string
	project      string
	restart      bool
	noResume     bool
	protoMode    bool
	v3Mode       bool
	initSource   string
	initABI      string
	initName     string
	initAddress  string
	initChainID  string
	initStartBlk string
	initEndBlk   string
	cpuprofile   string
	pageSize     string
}

func parseArgs(args []string) (*parsedArgs, error) {
	p := &parsedArgs{
		protoMode: true,
	}
	positional := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-r", "--restart":
			p.restart = true
		case "--no-resume":
			p.noResume = true
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
		case "--version":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--version requires a value (1, 2, or 3)")
			}
			switch args[i] {
			case "1":
				p.protoMode = false
				p.v3Mode = false
			case "2":
				p.protoMode = true
				p.v3Mode = false
			case "3":
				p.protoMode = true
				p.v3Mode = true
			default:
				return nil, fmt.Errorf("--version must be 1, 2, or 3 (got %q)", args[i])
			}
		case "--v1":
			p.protoMode = false
		case "--v3":
			p.v3Mode = true
		case "-p", "--pagesize":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--pagesize requires a value")
			}
			p.pageSize = args[i]
		default:
			if !strings.HasPrefix(a, "-") {
				positional = append(positional, a)
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
			fmt.Fprintln(os.Stderr, "usage: sqd-go start <project-dir|config.yaml|config.yml> [--restart|--no-resume] [--start-block <n>] [--end-block <n>] [--blockchain <id|name>]")
			return 2
		}
		return runStartPipeline(p.project, p.restart, p.noResume, p.protoMode, p.v3Mode, p.initStartBlk, p.initEndBlk, p.initChainID, p.cpuprofile, p.pageSize)

	case "dev":
		if p.project == "" {
			fmt.Fprintln(os.Stderr, "usage: sqd-go dev <project-dir|config.yaml|config.yml> [--restart|--no-resume]")
			return 2
		}
		return runDev(p.project, p.restart, p.noResume, p.protoMode, p.v3Mode, p.initStartBlk, p.initEndBlk, p.initChainID, p.cpuprofile, p.pageSize)

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
		return runStartPipeline(p.command, false, p.noResume, p.protoMode, p.v3Mode, p.initStartBlk, p.initEndBlk, p.initChainID, p.cpuprofile, p.pageSize)
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
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func envOrDefaultInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		fmt.Sscanf(v, "%d", &n)
		return n
	}
	return defaultVal
}

func envOrDefaultBool(key string, defaultVal bool) bool {
	if v := strings.TrimSpace(strings.ToLower(os.Getenv(key))); v != "" {
		return v == "1" || v == "true" || v == "yes" || v == "on"
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
  --no-resume           (dev/start) Drop DB and start from configured block
  --abi, -a             (init) Path to ABI JSON file
  --name, -n            (init) Contract name
  --address, --addr     (init) Contract address (hex)
  --blockchain, -b      (init/start) Chain ID or name (default: 1)
  --start-block, -s     (init/start) Start block (default: 0)
  --end-block, -e       (start) End block (0 for infinite)
  --pagesize, -p        (start/dev) Fixed page size range to fetch (default: 0 for dynamic)
  --version             (start/dev) Pipeline version: 1 (legacy parsed), 2 (proto, default), 3 (discovery prefetch)
  --v1                  (start/dev) Alias for --version 1
  --v3                  (start/dev) Alias for --version 3

Examples:
  sqd-go
  sqd-go init
  sqd-go codegen examples/uniswap
  sqd-go start examples/uniswap
  sqd-go start examples/uniswap --blockchain polygon --start-block 80000000 --restart
  sqd-go start examples/uniswap --no-resume
  sqd-go dev examples/uniswap --restart
  sqd-go stop
  sqd-go init contract-import local --abi erc20.json --name USDC --address 0xA0...
`)
}
