package cli

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/franz101/sqd-go/internal/codegen"
	"github.com/franz101/sqd-go/internal/config"
	"gopkg.in/yaml.v3"
)

type initOption int

const (
	initOptionABIFile initOption = iota
	initOptionERC20Template
)

type interactiveInitRequest struct {
	ProjectDir   string
	ProjectName  string
	Option       initOption
	ABIFile      string
	ContractName string
	ChainID      uint64
	StartBlock   uint64
	Address      string
}

func runInteractiveInit() int {
	req, err := promptInteractiveInit(os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init: %v\n", err)
		return 1
	}
	configPath, eventCount, err := initializeFromRequest(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init: %v\n", err)
		return 1
	}
	if code := runInitWithConfig(configPath); code != 0 {
		return code
	}

	fmt.Printf("initialized %s (%d events)\n", configPath, eventCount)
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Printf("  cd %s\n", req.ProjectDir)
	fmt.Println("  sqd-go start . --state --restart")
	return 0
}

func runInitTemplate(p *parsedArgs) int {
	templateArg := strings.TrimSpace(p.initSource)
	template := strings.ToLower(templateArg)
	projectDir := strings.TrimSpace(p.project)
	if template == "" {
		template = "erc20"
	}
	if template != "erc20" && projectDir == "" {
		projectDir = templateArg
		template = "erc20"
	}
	if template != "erc20" {
		fmt.Fprintf(os.Stderr, "unsupported template %q (only \"erc20\" is supported)\n", template)
		return 2
	}

	if projectDir == "" {
		projectDir = "sqd-indexer"
		if p.initName != "" {
			projectDir = defaultProjectDirFromName(p.initName)
		}
	}
	projectName := p.initName
	if projectName == "" {
		projectName = deriveProjectName(projectDir)
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
	if err := validateOptionalEVMAddress(p.initAddress); err != nil {
		fmt.Fprintf(os.Stderr, "invalid address: %v\n", err)
		return 2
	}

	cfg := erc20TemplateConfig(projectName)
	cfg.Chains[0].ID = chainID
	cfg.Chains[0].StartBlock = startBlock
	if p.initAddress != "" {
		cfg.Chains[0].Contracts[0].Address = config.Address{p.initAddress}
	}

	configPath, err := writeInitProjectFiles(projectDir, cfg, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "write project: %v\n", err)
		return 1
	}
	if code := runInitWithConfig(configPath); code != 0 {
		return code
	}
	fmt.Printf("initialized %s (2 events)\n", configPath)
	return 0
}

func promptInteractiveInit(in io.Reader, out io.Writer) (*interactiveInitRequest, error) {
	reader := bufio.NewReader(in)

	fmt.Fprintln(out, "sqd-go init")
	fmt.Fprintln(out, "Blockchain ecosystem: EVM")
	fmt.Fprintln(out)

	projectDir, err := promptText(reader, out, "Specify a folder name", "sqd-indexer", validateProjectDir)
	if err != nil {
		return nil, err
	}
	optionIndex, err := promptSelect(reader, out, "Choose an initialization option", []string{
		"From ABI File - Use your own ABI file",
		"Template: ERC20",
	}, 0)
	if err != nil {
		return nil, err
	}

	req := &interactiveInitRequest{
		ProjectDir:  projectDir,
		ProjectName: deriveProjectName(projectDir),
		Option:      initOption(optionIndex),
	}
	if req.Option == initOptionERC20Template {
		return req, nil
	}

	abiFile, err := promptText(reader, out, "ABI file path", "", validateExistingFile)
	if err != nil {
		return nil, err
	}
	req.ABIFile = abiFile

	contractName, err := promptText(reader, out, "Contract name", defaultContractName(abiFile), validateRequired)
	if err != nil {
		return nil, err
	}
	req.ContractName = contractName

	chainID, err := promptChainID(reader, out)
	if err != nil {
		return nil, err
	}
	req.ChainID = chainID

	startBlock, err := promptUint64(reader, out, "Start block", 0, false)
	if err != nil {
		return nil, err
	}
	req.StartBlock = startBlock

	address, err := promptText(reader, out, "Contract address (ENTER to skip)", "", validateOptionalEVMAddress)
	if err != nil {
		return nil, err
	}
	req.Address = address

	return req, nil
}

func initializeFromRequest(req *interactiveInitRequest) (string, int, error) {
	switch req.Option {
	case initOptionABIFile:
		abiData, err := os.ReadFile(req.ABIFile)
		if err != nil {
			return "", 0, fmt.Errorf("read abi: %w", err)
		}
		events, err := extractEventsFromABI(abiData)
		if err != nil {
			return "", 0, fmt.Errorf("parse abi: %w", err)
		}
		if len(events) == 0 {
			return "", 0, fmt.Errorf("no events found in ABI: the ABI has no \"type\":\"event\" entries; router/library contracts emit no events — use the ABI of the contract that emits them (e.g. for Uniswap V2, the Factory or a Pair, not the Router)")
		}
		eventConfigs := make([]config.EventConfig, len(events))
		for i, event := range events {
			eventConfigs[i] = config.EventConfig{Event: event}
		}

		var address config.Address
		if req.Address != "" {
			address = config.Address{req.Address}
		}
		cfg := &config.Config{
			Name:      req.ProjectName,
			Ecosystem: stringPtr("evm"),
			Chains: []config.Chain{{
				ID:         req.ChainID,
				StartBlock: req.StartBlock,
				Contracts: []config.ChainContractConfig{{
					Name:    req.ContractName,
					Address: address,
					Events:  eventConfigs,
				}},
			}},
		}
		configPath, err := writeInitProjectFiles(req.ProjectDir, cfg, req.ABIFile)
		return configPath, len(events), err

	case initOptionERC20Template:
		cfg := erc20TemplateConfig(req.ProjectName)
		configPath, err := writeInitProjectFiles(req.ProjectDir, cfg, "")
		return configPath, 2, err

	default:
		return "", 0, fmt.Errorf("unknown init option")
	}
}

func erc20TemplateConfig(projectName string) *config.Config {
	return &config.Config{
		Name:      projectName,
		Ecosystem: stringPtr("evm"),
		Chains: []config.Chain{{
			ID:         1,
			StartBlock: 0,
			Contracts: []config.ChainContractConfig{{
				Name:    "ERC20",
				Address: config.Address{"0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"},
				Events: []config.EventConfig{
					{Event: "Transfer(address indexed from, address indexed to, uint256 value)"},
					{Event: "Approval(address indexed owner, address indexed spender, uint256 value)"},
				},
			}},
		}},
	}
}

func extractEventsFromABI(data []byte) ([]string, error) {
	_, err := abi.JSON(bytes.NewReader(data))
	if err != nil {
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

func writeInitProjectFiles(projectDir string, cfg *config.Config, abiFile string) (string, error) {
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		return "", fmt.Errorf("create project dir: %w", err)
	}
	for _, name := range []string{"config.yaml", "config.yml"} {
		path := filepath.Join(projectDir, name)
		if _, err := os.Stat(path); err == nil {
			return "", fmt.Errorf("%s already exists", path)
		} else if err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("stat %s: %w", path, err)
		}
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal config: %w", err)
	}
	configPath := filepath.Join(projectDir, "config.yaml")
	if err := writeFileExclusive(configPath, data, 0o644); err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	if err := writeScaffoldSupportFiles(projectDir, cfg.Name); err != nil {
		return "", err
	}
	if abiFile != "" {
		if err := copyABIToProject(projectDir, abiFile); err != nil {
			return "", err
		}
	}
	return configPath, nil
}

func writeScaffoldSupportFiles(projectDir, projectName string) error {
	envPath := filepath.Join(projectDir, ".env")
	if err := writeFileIfMissing(envPath, []byte(clickHouseEnv(projectName)), 0o600); err != nil {
		return fmt.Errorf("write .env: %w", err)
	}
	composePath := filepath.Join(projectDir, "compose.yml")
	if err := writeFileIfMissing(composePath, []byte(clickHouseCompose(projectName)), 0o644); err != nil {
		return fmt.Errorf("write compose.yml: %w", err)
	}
	return nil
}

func copyABIToProject(projectDir, abiFile string) error {
	raw, err := os.ReadFile(abiFile)
	if err != nil {
		return fmt.Errorf("read abi for copy: %w", err)
	}
	abiDir := filepath.Join(projectDir, "abis")
	if err := os.MkdirAll(abiDir, 0o755); err != nil {
		return fmt.Errorf("create abi dir: %w", err)
	}
	target := filepath.Join(abiDir, filepath.Base(abiFile))
	if err := writeFileIfMissing(target, raw, 0o644); err != nil {
		return fmt.Errorf("copy abi: %w", err)
	}
	return nil
}

func clickHouseEnv(projectName string) string {
	return fmt.Sprintf(`CLICKHOUSE_HOST=127.0.0.1
CLICKHOUSE_HTTP_PORT=8123
CLICKHOUSE_NATIVE_PORT=9000
CLICKHOUSE_USER=default
CLICKHOUSE_PASSWORD=sqd-clickhouse
CLICKHOUSE_DATABASE=%s
`, projectName)
}

func clickHouseCompose(projectName string) string {
	return fmt.Sprintf(`name: %s

services:
  clickhouse:
    image: clickhouse/clickhouse-server:latest
    environment:
      CLICKHOUSE_USER: "${CLICKHOUSE_USER:-default}"
      CLICKHOUSE_PASSWORD: "${CLICKHOUSE_PASSWORD:-sqd-clickhouse}"
      CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT: "1"
    ports:
      - "${CLICKHOUSE_HTTP_PORT:-8123}:8123"
      - "${CLICKHOUSE_NATIVE_PORT:-9000}:9000"
    volumes:
      - clickhouse_data:/var/lib/clickhouse
    ulimits:
      nofile:
        soft: 262144
        hard: 262144

volumes:
  clickhouse_data:
`, composeProjectName(projectName))
}

func writeFileIfMissing(path string, data []byte, perm os.FileMode) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return writeFileExclusive(path, data, perm)
}

func writeFileExclusive(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

func promptText(reader *bufio.Reader, out io.Writer, label, defaultValue string, validate func(string) error) (string, error) {
	for {
		if defaultValue == "" {
			fmt.Fprintf(out, "%s: ", label)
		} else {
			fmt.Fprintf(out, "%s [%s]: ", label, defaultValue)
		}
		value, err := readPromptLine(reader)
		if err != nil {
			return "", err
		}
		if value == "" {
			value = defaultValue
		}
		if validate != nil {
			if err := validate(value); err != nil {
				fmt.Fprintf(out, "%v\n", err)
				continue
			}
		}
		return value, nil
	}
}

func promptSelect(reader *bufio.Reader, out io.Writer, label string, options []string, defaultIndex int) (int, error) {
	fmt.Fprintln(out, label)
	for i, option := range options {
		fmt.Fprintf(out, "  %d) %s\n", i+1, option)
	}
	for {
		prompt := "Enter choice"
		if defaultIndex >= 0 && defaultIndex < len(options) {
			prompt = fmt.Sprintf("%s [%d]", prompt, defaultIndex+1)
		}
		fmt.Fprintf(out, "%s: ", prompt)
		value, err := readPromptLine(reader)
		if err != nil {
			return 0, err
		}
		if value == "" && defaultIndex >= 0 && defaultIndex < len(options) {
			return defaultIndex, nil
		}
		n, err := strconv.Atoi(value)
		if err == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
		for i, option := range options {
			if strings.EqualFold(value, option) {
				return i, nil
			}
		}
		fmt.Fprintf(out, "Please enter a number between 1 and %d.\n", len(options))
	}
}

func promptChainID(reader *bufio.Reader, out io.Writer) (uint64, error) {
	option, err := promptSelect(reader, out, "Choose EVM network", []string{
		"Ethereum Mainnet (1)",
		"Polygon Mainnet (137)",
		"Custom Chain ID",
	}, 0)
	if err != nil {
		return 0, err
	}
	switch option {
	case 0:
		return 1, nil
	case 1:
		return 137, nil
	default:
		return promptUint64(reader, out, "Custom chain ID", 1, true)
	}
}

func promptUint64(reader *bufio.Reader, out io.Writer, label string, defaultValue uint64, nonZero bool) (uint64, error) {
	returnValue := defaultValue
	validate := func(value string) error {
		n, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return fmt.Errorf("%s must be an unsigned integer", label)
		}
		if nonZero && n == 0 {
			return fmt.Errorf("%s must be greater than zero", label)
		}
		returnValue = n
		return nil
	}
	_, err := promptText(reader, out, label, strconv.FormatUint(defaultValue, 10), validate)
	if err != nil {
		return 0, err
	}
	return returnValue, nil
}

func readPromptLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && line != "" {
			return strings.TrimSpace(line), nil
		}
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func validateProjectDir(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("folder name is required")
	}
	if strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return fmt.Errorf("folder name cannot contain whitespace")
	}
	clean := filepath.Clean(value)
	if clean == "." || clean == string(filepath.Separator) {
		return fmt.Errorf("folder name must create a project directory")
	}
	if info, err := os.Stat(clean); err == nil && !info.IsDir() {
		return fmt.Errorf("%s exists and is not a directory", clean)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, part := range strings.FieldsFunc(clean, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("folder path cannot contain empty, current, or parent directory segments")
		}
		if strings.ContainsAny(part, ":*?\"'<>|") {
			return fmt.Errorf("folder names cannot contain any of: / \\ : * ? \" ' < > |")
		}
	}
	for _, name := range []string{"config.yaml", "config.yml"} {
		if _, err := os.Stat(filepath.Join(clean, name)); err == nil {
			return fmt.Errorf("%s already exists", filepath.Join(clean, name))
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func validateExistingFile(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("file path is required")
	}
	info, err := os.Stat(value)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory", value)
	}
	return nil
}

func validateRequired(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("value is required")
	}
	return nil
}

func validateOptionalEVMAddress(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if !(strings.HasPrefix(value, "0x") || strings.HasPrefix(value, "0X")) || len(value) != 42 {
		return fmt.Errorf("address must be a 20-byte hex value like 0x0000000000000000000000000000000000000000")
	}
	if _, err := hex.DecodeString(value[2:]); err != nil {
		return fmt.Errorf("address must be valid hex")
	}
	return nil
}

func defaultContractName(abiFile string) string {
	base := strings.TrimSuffix(filepath.Base(abiFile), filepath.Ext(abiFile))
	base = strings.TrimSpace(base)
	if base == "" {
		return "Contract"
	}
	if strings.EqualFold(base, "erc20") {
		return "ERC20"
	}
	parts := strings.FieldsFunc(base, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	var b strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		runes := []rune(strings.ToLower(part))
		runes[0] = unicode.ToUpper(runes[0])
		b.WriteString(string(runes))
	}
	if b.Len() == 0 {
		return "Contract"
	}
	return b.String()
}

func defaultProjectDirFromName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "sqd-indexer"
	}
	return out
}

func deriveProjectName(projectDir string) string {
	name := filepath.Base(filepath.Clean(projectDir))
	if name == "." || name == string(filepath.Separator) || name == "" {
		return "sqd-indexer"
	}
	return name
}

func composeProjectName(projectName string) string {
	name := strings.ToLower(projectName)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	clean := strings.Trim(b.String(), "-_")
	if clean == "" {
		clean = "indexer"
	}
	return "sqd-go-" + clean
}

func runInitWithConfig(configPath string) int {
	project, err := config.LoadProject(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return 1
	}

	packageName := goPackageName(project.Root)
	absRoot, err := filepath.Abs(project.Root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "absolute path: %v\n", err)
		return 1
	}
	importPath, err := scaffoldGeneratedImport(absRoot, packageName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "generated import path: %v\n", err)
		return 1
	}

	customSchemaPath := filepath.Join(project.Root, "custom_schema.go")
	schemaContent, processorContent, err := codegen.RenderStateScaffold(project.Config, packageName, importPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "render state scaffold: %v\n", err)
		return 1
	}

	if err := writeFileIfMissing(customSchemaPath, schemaContent, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write custom_schema.go: %v\n", err)
		return 1
	}
	fmt.Printf("created template: %s\n", customSchemaPath)

	// Write custom_processor.go
	customProcessorPath := filepath.Join(project.Root, "custom_processor.go")
	if err := writeFileIfMissing(customProcessorPath, processorContent, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write custom_processor.go: %v\n", err)
		return 1
	}
	fmt.Printf("created template: %s\n", customProcessorPath)

	return 0
}

func scaffoldGeneratedImport(projectRoot, packageName string) (string, error) {
	moduleRoot, modulePath, err := findGoModule(projectRoot)
	if err != nil {
		// A standalone project gets a matching module from `start --state`.
		return packageName + "/generated", nil
	}
	relPath, err := filepath.Rel(moduleRoot, projectRoot)
	if err != nil {
		return "", err
	}
	if relPath == "." {
		return filepath.ToSlash(filepath.Join(modulePath, "generated")), nil
	}
	return filepath.ToSlash(filepath.Join(modulePath, relPath, "generated")), nil
}

func goPackageName(dir string) string {
	name := deriveProjectName(dir)
	name = strings.ReplaceAll(name, "-", "_")
	var sb strings.Builder
	for i, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (r >= '0' && r <= '9' && i > 0) {
			sb.WriteRune(r)
		}
	}
	res := sb.String()
	if res == "" {
		return "project"
	}
	return strings.ToLower(res)
}

func findGoModule(dir string) (moduleRoot string, modulePath string, err error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}
	curr := absDir
	for {
		goModPath := filepath.Join(curr, "go.mod")
		if _, err := os.Stat(goModPath); err == nil {
			// Found go.mod! Let's read it.
			data, err := os.ReadFile(goModPath)
			if err != nil {
				return "", "", err
			}
			lines := strings.Split(string(data), "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "module ") {
					mod := strings.TrimSpace(strings.TrimPrefix(line, "module "))
					return curr, mod, nil
				}
			}
			return "", "", fmt.Errorf("no module declaration in %s", goModPath)
		}
		parent := filepath.Dir(curr)
		if parent == curr {
			break
		}
		curr = parent
	}
	return "", "", fmt.Errorf("could not find go.mod in parent directories of %s", absDir)
}
