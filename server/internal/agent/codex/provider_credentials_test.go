package codex

import (
	"os"
	"path/filepath"
	"testing"

	"mindfs/server/internal/testutil"
)

const magpieProviderConfig = `model_provider = "magpie"

[model_providers.magpie]
name = "magpie"
base_url = "http://magpie.home.arpa:3425/v1"
model_catalog_url = "http://magpie.home.arpa:3425/v1/codex/models"
env_key = "MAGPIE_API_KEY"
wire_api = "responses"
`

func writeCodexHome(t *testing.T, configText, dotenv string) {
	t.Helper()
	root := t.TempDir()
	testutil.IsolateUserDirs(t, root)
	dir := filepath.Join(root, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	if dotenv != "" {
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(dotenv), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A provider block that names its key through env_key (the codex-recommended
// shape, and what every magpie tree uses) carries no inline token. Without this
// resolution the model supplement silently falls back to api-providers.json —
// an IP base that is unreachable on a machine without that route, so codex
// reports zero models in the UI.
func TestCodexProviderCredentialsResolvesEnvKeyFromDotenv(t *testing.T) {
	writeCodexHome(t, magpieProviderConfig, "MAGPIE_API_KEY=sk-from-dotenv\n")
	t.Setenv("MAGPIE_API_KEY", "")
	base, key := codexProviderCredentials()
	if base != "http://magpie.home.arpa:3425/v1" || key != "sk-from-dotenv" {
		t.Fatalf("credentials = %q/%q, want magpie base + dotenv key", base, key)
	}
}

func TestCodexProviderCredentialsProcessEnvBeatsDotenv(t *testing.T) {
	writeCodexHome(t, magpieProviderConfig, "MAGPIE_API_KEY=sk-from-dotenv\n")
	t.Setenv("MAGPIE_API_KEY", "sk-from-env")
	_, key := codexProviderCredentials()
	if key != "sk-from-env" {
		t.Fatalf("key = %q, want process env to win", key)
	}
}

func TestCodexProviderCredentialsInlineTokenBeatsEnvKey(t *testing.T) {
	cfg := `model_provider = "magpie"

[model_providers.magpie]
name = "magpie"
base_url = "http://magpie.home.arpa:3425/v1"
experimental_bearer_token = "sk-inline"
env_key = "MAGPIE_API_KEY"
`
	writeCodexHome(t, cfg, "MAGPIE_API_KEY=sk-from-dotenv\n")
	_, key := codexProviderCredentials()
	if key != "sk-inline" {
		t.Fatalf("key = %q, want inline token", key)
	}
}

// Nothing resolvable for the env-named key must not produce a half-credential
// pair (base without key), which would take the fetch path and only log an error.
func TestCodexProviderCredentialsUnresolvableEnvKeyDoesNotReturnBaseAlone(t *testing.T) {
	writeCodexHome(t, magpieProviderConfig, "")
	t.Setenv("MAGPIE_API_KEY", "")
	base, key := codexProviderCredentials()
	if base != "" || key != "" {
		t.Fatalf("credentials = %q/%q, want empty so the fallback path runs", base, key)
	}
}
