package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvOrDefault(t *testing.T) {
	tests := []struct {
		name       string
		key        string
		defaultVal string
		setup      func()
		expected   string
	}{
		{
			name:       "env not set, returns default",
			key:        "TEST_VAR_NOT_SET",
			defaultVal: "default_value",
			expected:   "default_value",
		},
		{
			name:       "env set, returns env value",
			key:        "TEST_VAR_SET",
			defaultVal: "default_value",
			setup: func() {
				os.Setenv("TEST_VAR_SET", "env_value")
			},
			expected: "env_value",
		},
		{
			name:       "empty env returns empty (honors explicit empty)",
			key:        "TEST_VAR_EMPTY",
			defaultVal: "default_value",
			setup: func() {
				os.Setenv("TEST_VAR_EMPTY", "")
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup()
				defer os.Unsetenv(tt.key)
			}
			result := envOrDefault(tt.key, tt.defaultVal)
			if result != tt.expected {
				t.Errorf("envOrDefault(%q, %q) = %q, want %q", tt.key, tt.defaultVal, result, tt.expected)
			}
		})
	}
}

func TestEnvOrDefaultInt(t *testing.T) {
	tests := []struct {
		name       string
		key        string
		defaultVal int
		setup      func()
		expected   int
	}{
		{
			name:       "env not set, returns default",
			key:        "TEST_INT_NOT_SET",
			defaultVal: 42,
			expected:   42,
		},
		{
			name:       "env set with valid int",
			key:        "TEST_INT_SET",
			defaultVal: 42,
			setup: func() {
				os.Setenv("TEST_INT_SET", "100")
			},
			expected: 100,
		},
		{
			name:       "env set with invalid int, returns default",
			key:        "TEST_INT_INVALID",
			defaultVal: 42,
			setup: func() {
				os.Setenv("TEST_INT_INVALID", "not_a_number")
			},
			expected: 42,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup()
				defer os.Unsetenv(tt.key)
			}
			result := envOrDefaultInt(tt.key, tt.defaultVal)
			if result != tt.expected {
				t.Errorf("envOrDefaultInt(%q, %d) = %d, want %d", tt.key, tt.defaultVal, result, tt.expected)
			}
		})
	}
}

func TestEnvOrDefaultBool(t *testing.T) {
	tests := []struct {
		name       string
		key        string
		defaultVal bool
		setup      func()
		expected   bool
	}{
		{
			name:       "env not set, returns default",
			key:        "TEST_BOOL_NOT_SET",
			defaultVal: true,
			expected:   true,
		},
		{
			name:       "env set with '1', returns true",
			key:        "TEST_BOOL_TRUE",
			defaultVal: false,
			setup: func() {
				os.Setenv("TEST_BOOL_TRUE", "1")
			},
			expected: true,
		},
		{
			name:       "env set with '0', returns false",
			key:        "TEST_BOOL_FALSE",
			defaultVal: true,
			setup: func() {
				os.Setenv("TEST_BOOL_FALSE", "0")
			},
			expected: false,
		},
		{
			name:       "env set with 'true', returns true",
			key:        "TEST_BOOL_TRUE_STR",
			defaultVal: false,
			setup: func() {
				os.Setenv("TEST_BOOL_TRUE_STR", "true")
			},
			expected: true,
		},
		{
			name:       "env set with 'false', returns false",
			key:        "TEST_BOOL_FALSE_STR",
			defaultVal: true,
			setup: func() {
				os.Setenv("TEST_BOOL_FALSE_STR", "false")
			},
			expected: false,
		},
		{
			name:       "env set with invalid value, returns default",
			key:        "TEST_BOOL_INVALID",
			defaultVal: true,
			setup: func() {
				os.Setenv("TEST_BOOL_INVALID", "invalid")
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup()
				defer os.Unsetenv(tt.key)
			}
			result := envOrDefaultBool(tt.key, tt.defaultVal)
			if result != tt.expected {
				t.Errorf("envOrDefaultBool(%q, %t) = %t, want %t", tt.key, tt.defaultVal, result, tt.expected)
			}
		})
	}
}

func TestStringPtr(t *testing.T) {
	tests := []struct {
		input    string
		expected *string
	}{
		{"hello", &[]string{"hello"}[0]},
		{"", &[]string{""}[0]},
		{"test", &[]string{"test"}[0]},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := stringPtr(tt.input)
			if result == nil {
				t.Fatal("stringPtr returned nil")
			}
			if *result != tt.input {
				t.Errorf("stringPtr(%q) = %q, want %q", tt.input, *result, tt.input)
			}
		})
	}
}

func TestPtrStr(t *testing.T) {
	tests := []struct {
		name     string
		input    *string
		expected string
	}{
		{"valid pointer", &[]string{"hello"}[0], "hello"},
		{"empty string pointer", &[]string{""}[0], ""},
		{"nil pointer", nil, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ptrStr(tt.input)
			if result != tt.expected {
				t.Errorf("ptrStr(%v) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestChainIDFromName(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		expected    uint64
		expectError bool
	}{
		{"ethereum", "ethereum", 1, false},
		{"mainnet", "mainnet", 1, false},
		{"1", "1", 1, false},
		{"polygon", "polygon", 137, false},
		{"137", "137", 137, false},
		{"matic", "matic", 137, false},
		{"invalid", "invalid_chain", 0, true},
		// Empty input defaults to mainnet (chain 1) — this is the deliberate
		// fallback used when --blockchain is omitted from `sqd-go init`.
		{"empty", "", 1, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := chainIDFromName(tt.input)
			if tt.expectError {
				if err == nil {
					t.Errorf("chainIDFromName(%q) expected error, got nil", tt.input)
				}
			} else {
				if err != nil {
					t.Errorf("chainIDFromName(%q) unexpected error: %v", tt.input, err)
				}
				if result != tt.expected {
					t.Errorf("chainIDFromName(%q) = %d, want %d", tt.input, result, tt.expected)
				}
			}
		})
	}
}

func TestParseUintFlag(t *testing.T) {
	tests := []struct {
		name        string
		flag        string
		value       string
		defaultVal  uint64
		expected    uint64
		expectError bool
	}{
		{
			name:        "valid number",
			flag:        "--test-flag",
			value:       "12345",
			defaultVal:  0,
			expected:    12345,
			expectError: false,
		},
		{
			name:        "empty string returns default",
			flag:        "--test-flag",
			value:       "",
			defaultVal:  1000,
			expected:    1000,
			expectError: false,
		},
		{
			name:        "invalid number",
			flag:        "--test-flag",
			value:       "not_a_number",
			defaultVal:  0,
			expectError: true,
		},
		{
			name:        "negative number",
			flag:        "--test-flag",
			value:       "-100",
			defaultVal:  0,
			expectError: true,
		},
		{
			name:        "large number",
			flag:        "--test-flag",
			value:       "18446744073709551615", // max uint64
			defaultVal:  0,
			expected:    18446744073709551615,
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseUintFlag(tt.flag, tt.value, tt.defaultVal)
			if tt.expectError {
				if err == nil {
					t.Errorf("parseUintFlag(%q, %q, %d) expected error, got nil", tt.flag, tt.value, tt.defaultVal)
				}
			} else {
				if err != nil {
					t.Errorf("parseUintFlag(%q, %q, %d) unexpected error: %v", tt.flag, tt.value, tt.defaultVal, err)
				}
				if result != tt.expected {
					t.Errorf("parseUintFlag(%q, %q, %d) = %d, want %d", tt.flag, tt.value, tt.defaultVal, result, tt.expected)
				}
			}
		})
	}
}

func TestUsage(t *testing.T) {
	result := usage()
	if result == "" {
		t.Error("usage() returned empty string")
	}
	if !strings.Contains(result, "sqd-go") {
		t.Error("usage() should contain 'sqd-go'")
	}
	if !strings.Contains(result, "init") {
		t.Error("usage() should contain 'init'")
	}
	if !strings.Contains(result, "start") {
		t.Error("usage() should contain 'start'")
	}
	if !strings.Contains(result, "codegen") {
		t.Error("usage() should contain 'codegen'")
	}
}

func TestParseArgsErrorCases(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		expectError bool
		errorCheck  func(error) bool
	}{
		{
			name:        "missing value for --abi",
			args:        []string{"init", "contract-import", "local", "--abi"},
			expectError: true,
			errorCheck: func(err error) bool {
				return err != nil && strings.Contains(err.Error(), "--abi requires a value")
			},
		},
		{
			name:        "missing value for --name",
			args:        []string{"init", "contract-import", "local", "--name"},
			expectError: true,
			errorCheck: func(err error) bool {
				return err != nil && strings.Contains(err.Error(), "--name requires a value")
			},
		},
		{
			name:        "missing value for --address",
			args:        []string{"init", "contract-import", "local", "--address"},
			expectError: true,
			errorCheck: func(err error) bool {
				return err != nil && strings.Contains(err.Error(), "--address requires a value")
			},
		},
		{
			name:        "missing value for --start-block",
			args:        []string{"init", "contract-import", "local", "--start-block"},
			expectError: true,
			errorCheck: func(err error) bool {
				return err != nil && strings.Contains(err.Error(), "--start-block requires a value")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseArgs(tt.args)
			if tt.expectError {
				if err == nil {
					t.Error("parseArgs expected error, got nil")
				} else if tt.errorCheck != nil && !tt.errorCheck(err) {
					t.Errorf("error check failed: %v", err)
				}
			} else {
				if err != nil {
					t.Errorf("parseArgs unexpected error: %v", err)
				}
			}
		})
	}
}

func TestParseArgsFlagVariations(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		checkFunc    func(*parsedArgs) bool
		expectError  bool
	}{
		{
			name: "short form restart",
			args: []string{"start", "project", "-r"},
			checkFunc: func(p *parsedArgs) bool {
				return p.restart && p.command == "start" && p.project == "project"
			},
		},
		{
			name: "long form restart",
			args: []string{"start", "project", "--restart"},
			checkFunc: func(p *parsedArgs) bool {
				return p.restart && p.command == "start" && p.project == "project"
			},
		},
		{
			name: "no-resume maps to restart",
			args: []string{"start", "project", "--no-resume"},
			checkFunc: func(p *parsedArgs) bool {
				return p.restart && p.command == "start" && p.project == "project"
			},
		},
		{
			name: "with state flag",
			args: []string{"start", "project", "--state"},
			checkFunc: func(p *parsedArgs) bool {
				return p.state && p.command == "start" && p.project == "project"
			},
		},
		{
			name: "with prefetch flag",
			args: []string{"start", "project", "--prefetch"},
			checkFunc: func(p *parsedArgs) bool {
				return p.prefetch && p.command == "start" && p.project == "project"
			},
		},
		{
			name: "with no-replay flag",
			args: []string{"start", "project", "--no-replay"},
			checkFunc: func(p *parsedArgs) bool {
				return p.noReplay && p.command == "start" && p.project == "project"
			},
		},
		{
			name: "with parallel-fetch flag",
			args: []string{"start", "project", "--parallel-fetch"},
			checkFunc: func(p *parsedArgs) bool {
				return p.parallelFetch && p.command == "start" && p.project == "project"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseArgs(tt.args)
			if tt.expectError {
				if err == nil {
					t.Error("parseArgs expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("parseArgs unexpected error: %v", err)
				}
				if !tt.checkFunc(result) {
					t.Errorf("parseArgs check failed for args: %v", tt.args)
				}
			}
		})
	}
}

func TestResolveColdCache(t *testing.T) {
	tests := []struct {
		name       string
		flagOff    bool
		configVal  *bool
		expected   bool
	}{
		{
			name:     "flag off, config nil",
			flagOff:  true,
			configVal: nil,
			expected: false,
		},
		{
			name:     "flag off, config false",
			flagOff:  true,
			configVal: boolPtr(false),
			expected: false,
		},
		{
			name:     "flag off, config true",
			flagOff:  true,
			configVal: boolPtr(true),
			expected: false,
		},
		{
			// Cold cache is ON by default when --no-cold-cache is not passed
			// and the config doesn't explicitly disable it.
			name:     "flag on, config nil",
			flagOff:  false,
			configVal: nil,
			expected: true,
		},
		{
			name:     "flag on, config false",
			flagOff:  false,
			configVal: boolPtr(false),
			expected: false,
		},
		{
			name:     "flag on, config true",
			flagOff:  false,
			configVal: boolPtr(true),
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := resolveColdCache(tt.flagOff, tt.configVal)
			if result != tt.expected {
				t.Errorf("resolveColdCache(%t, %v) = %t, want %t", tt.flagOff, tt.configVal, result, tt.expected)
			}
		})
	}
}

// Helper function for tests
func boolPtr(b bool) *bool {
	return &b
}

func TestLoadEnv(t *testing.T) {
	tmpDir := t.TempDir()
	envPath := filepath.Join(tmpDir, ".env")

	// Create a test .env file
	envContent := "VAR1=value1\nVAR2=value2\n# COMMENT\nVAR3=value3"
	if err := os.WriteFile(envPath, []byte(envContent), 0644); err != nil {
		t.Fatalf("failed to create .env file: %v", err)
	}

	// Load the environment
	loadEnv(envPath)

	// Check if variables are set
	if v1 := os.Getenv("VAR1"); v1 != "value1" {
		t.Errorf("VAR1 = %q, want 'value1'", v1)
	}
	if v2 := os.Getenv("VAR2"); v2 != "value2" {
		t.Errorf("VAR2 = %q, want 'value2'", v2)
	}
	if v3 := os.Getenv("VAR3"); v3 != "value3" {
		t.Errorf("VAR3 = %q, want 'value3'", v3)
	}

	// Clean up
	os.Unsetenv("VAR1")
	os.Unsetenv("VAR2")
	os.Unsetenv("VAR3")
}

func TestLoadEnvNonExistent(t *testing.T) {
	// Should not panic on non-existent file
	loadEnv("/nonexistent/path/.env")
}

func TestDefaultContractName(t *testing.T) {
	tests := []struct {
		name     string
		abiFile  string
		expected string
	}{
		{
			// "erc20" is special-cased to the canonical all-caps Go identifier.
			name:     "simple filename",
			abiFile:  "erc20.json",
			expected: "ERC20",
		},
		{
			// Words are title-cased from a lowercased base, so internal
			// capitalization in the source filename is not preserved.
			name:     "path with filename",
			abiFile:  "/path/to/MyContract.json",
			expected: "Mycontract",
		},
		{
			name:     "filename with extension",
			abiFile:  "contract.abi",
			expected: "Contract",
		},
		{
			name:     "complex path",
			abiFile:  "/home/user/projects/token_abi.json",
			expected: "TokenAbi",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := defaultContractName(tt.abiFile)
			if result != tt.expected {
				t.Errorf("defaultContractName(%q) = %q, want %q", tt.abiFile, result, tt.expected)
			}
		})
	}
}

func TestDefaultProjectDirFromName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		// Sanitization lowercases the name and replaces runs of
		// non-alphanumeric characters with a single dash (see wiki/INIT.md).
		{"simple name", "my_project", "my-project"},
		{"name with spaces", "my project", "my-project"},
		{"name with special chars", "project@123!", "project-123"},
		{"uppercase", "MyProject", "myproject"},
		{"empty string", "", "sqd-indexer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := defaultProjectDirFromName(tt.input)
			if result != tt.expected {
				t.Errorf("defaultProjectDirFromName(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestDeriveProjectName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"simple path", "/home/user/my_project", "my_project"},
		{"path with trailing slash", "/home/user/my_project/", "my_project"},
		{"current directory", ".", "sqd-indexer"},
		{"nested path", "/home/user/projects/my indexer", "my indexer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := deriveProjectName(tt.input)
			if result != tt.expected {
				t.Errorf("deriveProjectName(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestComposeProjectName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		// composeProjectName always prefixes with "sqd-go-" (see
		// wiki/INIT.md: "derived as sqd-go-<sanitized-project-name>").
		// Underscores are preserved as-is; only characters outside
		// [a-z0-9-_] are replaced with a dash.
		{"simple name", "my_project", "sqd-go-my_project"},
		{"with spaces", "my project", "sqd-go-my-project"},
		{"with underscores", "my_project", "sqd-go-my_project"},
		{"with multiple underscores", "my_test_project", "sqd-go-my_test_project"},
		{"already with hyphens", "my-project", "sqd-go-my-project"},
		{"empty string", "", "sqd-go-indexer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := composeProjectName(tt.input)
			if result != tt.expected {
				t.Errorf("composeProjectName(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestGoPackageName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"simple path", "/home/user/my_project", "my_project"},
		{"path with separator", "/home/user/my-project", "my_project"},
		// This project targets Linux/Unix only (Docker-based deployment, no
		// Windows build tags); path/filepath does not treat backslash as a
		// separator on this platform, so the whole string is one path
		// component with the backslashes stripped by the identifier filter.
		{"windows path", "\\home\\user\\my_project", "homeusermy_project"},
		{"current dir", ".", "sqd_indexer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := goPackageName(tt.input)
			if result != tt.expected {
				t.Errorf("goPackageName(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}