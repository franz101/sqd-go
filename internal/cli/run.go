package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/franz101/sqd-go/internal/codegen"
	"github.com/franz101/sqd-go/internal/config"
	"github.com/franz101/sqd-go/internal/database"
	"github.com/franz101/sqd-go/internal/ingestion"
	"gopkg.in/yaml.v3"
)

type parsedArgs struct {
	command string
	project string
	restart bool
	// init flags
	initSource   string
	initABI      string
	initName     string
	initAddress  string
	initChainID  string
	initStartBlk string
}

func parseArgs(args []string) (*parsedArgs, error) {
	p := &parsedArgs{}
	positional := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-r", "--restart":
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
			p.command = "init:" + positional[1] // init:contract-import or init:template
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
	loadEnv(".env")

	p, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse args: %v\n", err)
		return 2
	}

	if p.command == "" {
		return runStartPipeline("examples/polymarket", false)
	}

	switch p.command {
	case "codegen":
		if p.project == "" {
			fmt.Fprintln(os.Stderr, "usage: go run . codegen <project-dir|config.yaml|config.yml>")
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
			fmt.Fprintln(os.Stderr, "usage: go run . start <project-dir|config.yaml|config.yml> [--restart]")
			return 2
		}
		return runStartPipeline(p.project, p.restart)

	case "dev":
		if p.project == "" {
			fmt.Fprintln(os.Stderr, "usage: go run . dev <project-dir|config.yaml|config.yml> [--restart]")
			return 2
		}
		return runDev(p.project, p.restart)

	case "stop":
		return runStop()

	case "init:contract-import":
		return runInitContractImport(p)

	case "init:template":
		fmt.Fprintln(os.Stderr, "init template: not yet implemented")
		return 1

	case "-h", "--help", "help":
		fmt.Print(usage())
		return 0

	default:
		return runStartPipeline(p.command, false)
	}
}

func runDev(path string, restart bool) int {
	log.Printf("dev: loading project %s", path)
	project, err := config.LoadProject(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return 1
	}
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
			dcmd.Stdout = os.Stdout
			dcmd.Stderr = os.Stderr
			dcmd.Run()
		}()
	}

	return runStartPipeline(project.ConfigPath, restart)
}

func runStartPipeline(path string, restart bool) int {
	project, err := config.LoadProject(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return 1
	}
	outPath, err := codegen.GenerateProject(project)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codegen: %v\n", err)
		return 1
	}
	log.Printf("Generated %s", outPath)
	log.Printf("Loaded config: %s (ecosystem: %s)", project.Config.Name, ptrStr(project.Config.Ecosystem))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	opts := ingestion.Options{
		ClickHouseHost:     envOrDefault("CLICKHOUSE_HOST", "127.0.0.1"),
		ClickHousePort:     envOrDefaultInt("CLICKHOUSE_NATIVE_PORT", 9004),
		ClickHouseUser:     envOrDefault("CLICKHOUSE_USER", "default"),
		ClickHousePassword: envOrDefault("CLICKHOUSE_PASSWORD", "sqd-clickhouse"),
		ClickHouseDatabase: envOrDefault("CLICKHOUSE_DATABASE", project.Config.Name),
		PageSize:           uint64(envOrDefaultInt("DEV_PAGE_SIZE", 0)),
		StartBlock:         uint64(envOrDefaultInt("DEV_START_BLOCK", 0)),
		BlockCount:         uint64(envOrDefaultInt("DEV_BLOCK_COUNT", 0)),
		Restart:            restart,
		GeneratedSQLDir:    filepath.Dir(outPath),
		CursorMode:         envOrDefaultBool("DEV_CURSOR", true),
	}
	if err := ingestion.Run(ctx, project.Config, opts); err != nil {
		fmt.Fprintf(os.Stderr, "start: %v\n", err)
		return 1
	}
	return 0
}

func runStop() int {
	cwd, _ := os.Getwd()
	cf := findComposeFile(cwd)
	if cf != "" {
		cmd := exec.Command("docker", "compose", "-f", cf, "down", "-v")
		cmd.Dir = cwd
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "docker compose down: %v\n", err)
			return 1
		}
		log.Println("docker compose down OK")
	}

	host := envOrDefault("CLICKHOUSE_HOST", "127.0.0.1")
	port := envOrDefaultInt("CLICKHOUSE_NATIVE_PORT", 9004)
	user := envOrDefault("CLICKHOUSE_USER", "default")
	passwd := envOrDefault("CLICKHOUSE_PASSWORD", "sqd-clickhouse")
	db := envOrDefault("CLICKHOUSE_DATABASE", "default")

	ctx := context.Background()
	if err := database.DropClickHouseDatabase(ctx, host, port, user, passwd, db); err != nil {
		fmt.Fprintf(os.Stderr, "drop db: %v\n", err)
	}
	log.Printf("dropped ClickHouse database %s", db)
	return 0
}

func runInitContractImport(p *parsedArgs) int {
	if p.initABI == "" {
		fmt.Fprintln(os.Stderr, "usage: go run . init contract-import local --abi <file> --name <name> [--address <addr>]")
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
		projectDir = strings.ToLower(name)
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

	cfg := config.Config{
		Name:      name,
		Ecosystem: stringPtr("evm"),
		Chains: []config.Chain{{
			ID:         chainID,
			StartBlock: startBlock,
			Contracts: []config.ChainContractConfig{{
				Name:    name,
				Address: p.initAddress,
				Events:  eventConfigs,
			}},
		}},
	}
	if p.initAddress == "" {
		cfg.Chains[0].Contracts[0].Address = nil
	}
	data, err := yaml.Marshal(&cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal config: %v\n", err)
		return 1
	}

	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "create project dir: %v\n", err)
		return 1
	}
	configPath := filepath.Join(projectDir, "config.yaml")
	if _, err := os.Stat(configPath); err == nil {
		fmt.Fprintf(os.Stderr, "%s already exists\n", configPath)
		return 1
	} else if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "stat config: %v\n", err)
		return 1
	}
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write config: %v\n", err)
		return 1
	}
	fmt.Printf("initialized %s (%d events)\n", configPath, len(events))
	return 0
}

func extractEventsFromABI(data []byte) ([]string, error) {
	if _, err := abi.JSON(bytes.NewReader(data)); err != nil {
		return nil, fmt.Errorf("abi.JSON: %w", err)
	}
	var fields []struct {
		Type      string
		Name      string
		Inputs    abi.Arguments
		Anonymous bool
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("json ABI: %w", err)
	}
	var events []string
	used := make(map[string]struct{})
	for i, field := range fields {
		if field.Type != "event" {
			continue
		}
		if strings.TrimSpace(field.Name) == "" {
			return nil, fmt.Errorf("event at ABI index %d is missing a name", i)
		}
		name := abi.ResolveNameConflict(field.Name, func(s string) bool {
			_, ok := used[s]
			return ok
		})
		used[name] = struct{}{}
		inputs := append(abi.Arguments(nil), field.Inputs...)
		ev := abi.NewEvent(name, field.Name, field.Anonymous, inputs)
		events = append(events, eventSignature(ev))
	}
	return events, nil
}

func eventSignature(ev abi.Event) string {
	var params []string
	for _, input := range ev.Inputs {
		p := input.Type.String()
		if input.Indexed {
			p += " indexed"
		}
		if input.Name != "" {
			p += " " + input.Name
		}
		params = append(params, p)
	}
	return fmt.Sprintf("%s(%s)", ev.RawName, strings.Join(params, ", "))
}

func chainIDFromName(name string) (uint64, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "ethereum", "ethereum-mainnet", "mainnet", "1":
		return 1, nil
	case "polygon", "polygon-mainnet", "matic", "137":
		return 137, nil
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

func stringPtr(v string) *string {
	return &v
}

func findComposeFile(root string) string {
	for _, name := range []string{"compose.yml", "compose.yaml", "docker-compose.yml", "docker-compose.yaml"} {
		path := root + "/" + name
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

func usage() string {
	return strings.TrimSpace(`
sqd-go — EVM log indexer

Usage:
  go run . <command> [arguments]

Commands:
  codegen <path>       Validate config and generate manifest (path can be dir or config.yaml/yml)
  start   <path>       Run codegen, then start ingestion
  dev     <path>       codegen + docker compose up + start (dev mode)
  stop                 Stop local env (docker compose down + drop DB)
  init    contract-import local [path] --abi <file> --name <name> [--address <addr>]
                       Scaffold a new project from an ABI file
  help                 Show this help

Flags:
  --restart             (dev/start) Drop DB and re-index from scratch
  --abi, -a             (init) Path to ABI JSON file
  --name, -n            (init) Contract name
  --address, --addr     (init) Contract address (hex)
  --blockchain, -b      (init) Chain ID (default: 1)
  --start-block, -s     (init) Start block (default: 0)

Examples:
  go run . codegen examples/uniswap
  go run . start examples/uniswap
  go run . dev examples/uniswap --restart
  go run . stop
  go run . init contract-import local --abi erc20.json --name USDC --address 0xA0...
`)
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

func ptrStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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
