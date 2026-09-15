package config

import (
	"reflect"
	"strings"
	"testing"
)

// TestEveryRequiredVariableIsRefusedWhenUnset checks that all 10 required
// variables cause Load to report a problem when unset.
func TestEveryRequiredVariableIsRefusedWhenUnset(t *testing.T) {
	requiredVars := []string{
		"REPO", "APPROVER", "LEDGER_BUCKET",
		"LEDGER_APPLIED_PREFIX", "LEDGER_FAILED_PREFIX", "LEDGER_HEAD_KEY",
		"HEARTBEAT_KEY", "PLAN_DIGEST_PREFIX", "WORKDIR", "OP_TOKEN_FILE",
	}

	for _, varName := range requiredVars {
		t.Run(varName, func(t *testing.T) {
			// getenv that returns empty for everything
			getenv := func(name string) string {
				if name == varName {
					return "" // leave this one unset
				}
				return "dummy" // set all others
			}

			_, problems := Load(getenv)
			if len(problems) == 0 {
				t.Errorf("expected problems when %s is unset, got none", varName)
			}

			found := false
			for _, p := range problems {
				if strings.Contains(p, varName) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected problem mentioning %s, got: %v", varName, problems)
			}
		})
	}
}

// TestAnEmptyStringCountsAsUnset verifies that empty strings are treated the
// same as missing variables.
func TestAnEmptyStringCountsAsUnset(t *testing.T) {
	getenv := func(name string) string {
		switch name {
		case "REPO":
			return "" // empty counts as unset
		case "APPROVER", "LEDGER_BUCKET", "LEDGER_APPLIED_PREFIX",
			"LEDGER_FAILED_PREFIX", "LEDGER_HEAD_KEY", "HEARTBEAT_KEY",
			"PLAN_DIGEST_PREFIX", "WORKDIR", "OP_TOKEN_FILE":
			return "dummy"
		default:
			return ""
		}
	}

	_, problems := Load(getenv)

	found := false
	for _, p := range problems {
		if strings.Contains(p, "REPO") && strings.Contains(p, "unset") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected problem about REPO being unset, got: %v", problems)
	}
}

// TestLoadReportsEveryProblemNotJustTheFirst verifies that Load returns
// all problems, not just the first one.
func TestLoadReportsEveryProblemNotJustTheFirst(t *testing.T) {
	getenv := func(name string) string {
		// Return empty for first three required vars
		switch name {
		case "REPO", "APPROVER", "LEDGER_BUCKET":
			return ""
		default:
			return "dummy"
		}
	}

	_, problems := Load(getenv)

	if len(problems) < 3 {
		t.Errorf("expected at least 3 problems, got %d: %v", len(problems), problems)
	}

	// Check that all three are reported
	for _, varName := range []string{"REPO", "APPROVER", "LEDGER_BUCKET"} {
		found := false
		for _, p := range problems {
			if strings.Contains(p, varName) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected problem about %s in: %v", varName, problems)
		}
	}
}

// TestOnlyThreeVariablesHaveDefaults verifies that exactly three optional
// variables have default values.
func TestOnlyThreeVariablesHaveDefaults(t *testing.T) {
	// All required vars set, all others unset
	getenv := func(name string) string {
		switch name {
		case "REPO", "APPROVER", "LEDGER_BUCKET",
			"LEDGER_APPLIED_PREFIX", "LEDGER_FAILED_PREFIX", "LEDGER_HEAD_KEY",
			"HEARTBEAT_KEY", "PLAN_DIGEST_PREFIX", "WORKDIR", "OP_TOKEN_FILE":
			return "dummy"
		default:
			return ""
		}
	}

	cfg, problems := Load(getenv)

	if len(problems) != 0 {
		t.Fatalf("expected no problems with all required vars set, got: %v", problems)
	}

	// Check that the three defaults are applied
	if cfg.SecretsDir != "/secrets" {
		t.Errorf("expected SecretsDir=/secrets, got %q", cfg.SecretsDir)
	}
	if cfg.PluginDir != "/opt/tofu-providers" {
		t.Errorf("expected PluginDir=/opt/tofu-providers, got %q", cfg.PluginDir)
	}
	if cfg.ExpiryWarnDays != 30 {
		t.Errorf("expected ExpiryWarnDays=30, got %d", cfg.ExpiryWarnDays)
	}
}

// TestDriftCheckAcceptsOnlyZeroOrOne verifies that DRIFT_CHECK only accepts
// "0", "1", or unset. Any other value should produce an error.
func TestDriftCheckAcceptsOnlyZeroOrOne(t *testing.T) {
	tests := []struct {
		name        string
		driftCheck  string
		expectError bool
		expectBool  bool
	}{
		{"unset", "", false, false},   // unset -> DriftOnly=false
		{"zero", "0", false, false},   // "0" -> DriftOnly=false
		{"one", "1", false, true},     // "1" -> DriftOnly=true
		{"true", "true", true, false}, // "true" -> error
		{"yes", "yes", true, false},   // "yes" -> error
		{"two", "2", true, false},     // "2" -> error
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(name string) string {
				if name == "DRIFT_CHECK" {
					return tt.driftCheck
				}
				switch name {
				case "REPO", "APPROVER", "LEDGER_BUCKET",
					"LEDGER_APPLIED_PREFIX", "LEDGER_FAILED_PREFIX", "LEDGER_HEAD_KEY",
					"HEARTBEAT_KEY", "PLAN_DIGEST_PREFIX", "WORKDIR", "OP_TOKEN_FILE":
					return "dummy"
				default:
					return ""
				}
			}

			cfg, problems := Load(getenv)

			if tt.expectError {
				found := false
				for _, p := range problems {
					if strings.Contains(p, "DRIFT_CHECK") {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected error about DRIFT_CHECK=%q, got: %v", tt.driftCheck, problems)
				}
			} else {
				// No error expected
				for _, p := range problems {
					if strings.Contains(p, "DRIFT_CHECK") {
						t.Errorf("unexpected error about DRIFT_CHECK=%q: %s", tt.driftCheck, p)
					}
				}
				if cfg.DriftOnly != tt.expectBool {
					t.Errorf("expected DriftOnly=%v for DRIFT_CHECK=%q, got %v", tt.expectBool, tt.driftCheck, cfg.DriftOnly)
				}
			}
		})
	}
}

// TestHeartbeatPingURLAbsentMeansEmpty verifies that HEARTBEAT_PING_URL is a
// true optional-with-no-default: leaving it unset produces an empty
// HeartbeatPingURL and adds no problem, unlike a required var.
func TestHeartbeatPingURLAbsentMeansEmpty(t *testing.T) {
	getenv := func(name string) string {
		switch name {
		case "REPO", "APPROVER", "LEDGER_BUCKET",
			"LEDGER_APPLIED_PREFIX", "LEDGER_FAILED_PREFIX", "LEDGER_HEAD_KEY",
			"HEARTBEAT_KEY", "PLAN_DIGEST_PREFIX", "WORKDIR", "OP_TOKEN_FILE":
			return "dummy"
		default:
			return "" // HEARTBEAT_PING_URL included: left unset
		}
	}

	cfg, problems := Load(getenv)

	if len(problems) != 0 {
		t.Fatalf("expected no problems with HEARTBEAT_PING_URL unset, got: %v", problems)
	}
	if cfg.HeartbeatPingURL != "" {
		t.Errorf("expected HeartbeatPingURL to be empty when unset, got %q", cfg.HeartbeatPingURL)
	}
}

// TestRootsAreNotConfigurable uses reflection to verify that no field name
// suggests a configurable root path.
func TestRootsAreNotConfigurable(t *testing.T) {
	var cfg Config
	cfgType := reflect.TypeOf(cfg)

	forbiddenPatterns := []string{
		"CredentialsDir", "CredentialsPath", "CredentialsRoot",
		"PlatformDir", "PlatformPath", "PlatformRoot",
		"ProjectsDir", "ProjectsPath", "ProjectsRoot",
		"RootPath", "RootDir", "RootsDir",
	}

	for i := 0; i < cfgType.NumField(); i++ {
		fieldName := cfgType.Field(i).Name
		for _, pattern := range forbiddenPatterns {
			if fieldName == pattern {
				t.Errorf("field name %q suggests a configurable root path", fieldName)
			}
		}
	}
}

// TestConfigCarriesNoCredential uses reflection to verify that no field name
// looks like it holds a credential, except OPTokenFile which is a path.
func TestConfigCarriesNoCredential(t *testing.T) {
	var cfg Config
	cfgType := reflect.TypeOf(cfg)

	// These patterns indicate a field holds an actual credential value,
	// not a path or reference to one.
	credentialPatterns := []string{
		"Secret",     // clearly a secret value
		"Password",   // clearly a password
		"APIKey",     // clearly an API key value
		"API_Key",    // clearly an API key value
		"Credential", // clearly a credential
		"Bearer",     // clearly a bearer token
		"PrivateKey", // private key material (unless it ends with File or Path)
		"PublicKey",  // public key material (unless it ends with File or Path)
	}

	for i := 0; i < cfgType.NumField(); i++ {
		fieldName := cfgType.Field(i).Name

		// OPTokenFile is allowed because it's a path, not the credential itself
		if fieldName == "OPTokenFile" {
			continue
		}

		// Skip fields that are clearly paths or directories
		if strings.HasSuffix(fieldName, "File") || strings.HasSuffix(fieldName, "Dir") ||
			strings.HasSuffix(fieldName, "Path") {
			continue
		}

		for _, pattern := range credentialPatterns {
			if strings.Contains(fieldName, pattern) {
				t.Errorf("field name %q looks like it holds a credential", fieldName)
			}
		}
	}
}
