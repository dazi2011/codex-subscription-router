package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
)

func TestStoreBootstrapsPrimaryAndPersistsThreadAffinity(t *testing.T) {
	root := t.TempDir()
	primaryHome := filepath.Join(root, "primary")
	store, err := Open(filepath.Join(root, "mux"), primaryHome)
	if err != nil {
		t.Fatal(err)
	}
	accounts := store.Accounts()
	if len(accounts) != 1 || accounts[0].ID != "primary" || !accounts[0].Controller {
		t.Fatalf("unexpected bootstrap accounts: %#v", accounts)
	}
	added, err := store.AddAccount("Work")
	if err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join(added.CodexHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	wantConfig := "cli_auth_credentials_store = 'file'\nmcp_oauth_credentials_store = 'file'\n"
	if string(config) != wantConfig {
		t.Fatalf("unexpected isolated config: %q", config)
	}
	if err := store.SetThreadOwner("thread-1", added.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetThreadModel("thread-1", "daybreak-blue"); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(filepath.Join(root, "mux"), primaryHome)
	if err != nil {
		t.Fatal(err)
	}
	owner, ok := reopened.ThreadOwner("thread-1")
	if !ok || owner != added.ID {
		t.Fatalf("thread affinity was not persisted: owner=%q ok=%v", owner, ok)
	}
	if model, ok := reopened.ThreadModel("thread-1"); !ok || model != "daybreak-blue" {
		t.Fatalf("thread model was not persisted: model=%q ok=%v", model, ok)
	}
}

func TestAccountConfigInheritsManagedMCPAndPreservesLocalProjects(t *testing.T) {
	root := t.TempDir()
	primaryHome := filepath.Join(root, "primary")
	if err := os.MkdirAll(primaryHome, 0o700); err != nil {
		t.Fatal(err)
	}
	primaryConfig := `model = "gpt-test"

[mcp_servers.node_repl]
command = "/Applications/Codex Subscription Router.app/node_repl"

[mcp_servers.node_repl.env]
SKY_CUA_SERVICE_PATH = "/Applications/Codex Subscription Router Computer Use.app"

[projects."/primary-only"]
trust_level = "trusted"
`
	if err := os.WriteFile(filepath.Join(primaryHome, "config.toml"), []byte(primaryConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	muxRoot := filepath.Join(root, "mux")
	store, err := Open(muxRoot, primaryHome)
	if err != nil {
		t.Fatal(err)
	}
	added, err := store.AddAccount("Work")
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(added.CodexHome, "config.toml")
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(config)
	for _, expected := range []string{
		`cli_auth_credentials_store = 'file'`,
		`mcp_oauth_credentials_store = 'file'`,
		`model = 'gpt-test'`,
		`[mcp_servers.node_repl]`,
		`SKY_CUA_SERVICE_PATH = '/Applications/Codex Subscription Router Computer Use.app'`,
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("account config is missing %q:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "/primary-only") {
		t.Fatalf("primary project trust leaked into account config:\n%s", text)
	}

	text += `
[projects."/account-project"]
trust_level = "trusted"
`
	if err := os.WriteFile(configPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	primaryConfig = strings.ReplaceAll(primaryConfig, "gpt-test", "gpt-updated")
	if err := os.WriteFile(filepath.Join(primaryHome, "config.toml"), []byte(primaryConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(muxRoot, primaryHome); err != nil {
		t.Fatal(err)
	}
	config, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	text = string(config)
	if !strings.Contains(text, `model = 'gpt-updated'`) {
		t.Fatalf("managed config was not refreshed:\n%s", text)
	}
	if !strings.Contains(text, `[projects.'/account-project']`) {
		t.Fatalf("account project trust was not preserved:\n%s", text)
	}
}

func TestSyncManagedConfigPropagatesPluginsWithoutRestart(t *testing.T) {
	root := t.TempDir()
	primaryHome := filepath.Join(root, "primary")
	if err := os.MkdirAll(primaryHome, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(primaryHome, "config.toml")
	if err := os.WriteFile(configPath, []byte("model = \"before\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(root, "mux"), primaryHome)
	if err != nil {
		t.Fatal(err)
	}
	account, err := store.AddAccount("Work")
	if err != nil {
		t.Fatal(err)
	}
	updated := "model = \"after\"\n\n[plugins.\"browser@openai-bundled\"]\nenabled = true\n"
	if err := os.WriteFile(configPath, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.SyncManagedConfig(); err != nil {
		t.Fatal(err)
	}
	isolated, err := os.ReadFile(filepath.Join(account.CodexHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(isolated), `[plugins.'browser@openai-bundled']`) {
		t.Fatalf("plugin config did not propagate:\n%s", isolated)
	}
}

func TestUpdateAccountPreservesController(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root, filepath.Join(root, "primary"))
	if err != nil {
		t.Fatal(err)
	}
	label := "Personal"
	enabled := false
	account, err := store.UpdateAccount("primary", &label, &enabled)
	if err != nil {
		t.Fatal(err)
	}
	if account.Label != label || account.Enabled || !account.Controller {
		t.Fatalf("unexpected updated account: %#v", account)
	}
}

func TestThreadOwnerCompareAndSwapAndAccountRemoval(t *testing.T) {
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "mux"), filepath.Join(root, "primary"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.AddAccount("First")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AddAccount("Second")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetThreadOwner("thread-1", first.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapThreadOwner("thread-1", first.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceThreadMetadata(map[string]string{"thread-1": first.ID}, nil); err != nil {
		t.Fatal(err)
	}
	if owner, _ := store.ThreadOwner("thread-1"); owner != second.ID {
		t.Fatalf("history reconciliation stole migrated owner: %q", owner)
	}
	if err := store.CompareAndSwapThreadOwner("thread-1", first.ID, second.ID); err == nil {
		t.Fatal("stale owner update unexpectedly succeeded")
	}
	if _, err := store.RemoveAccount(second.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.ThreadOwner("thread-1"); ok {
		t.Fatal("removed account retained thread ownership")
	}
	if _, err := os.Stat(filepath.Dir(second.CodexHome)); !os.IsNotExist(err) {
		t.Fatalf("removed account home still exists: %v", err)
	}
}

func TestManagedConfigDoesNotRewriteUnchangedSecondary(t *testing.T) {
	root := t.TempDir()
	primaryHome := filepath.Join(root, "primary")
	if err := os.MkdirAll(primaryHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(primaryHome, "config.toml"), []byte("model = 'gpt-test'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(root, "mux"), primaryHome)
	if err != nil {
		t.Fatal(err)
	}
	account, err := store.AddAccount("Work")
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(account.CodexHome, "config.toml")
	past := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(configPath, past, past); err != nil {
		t.Fatal(err)
	}
	if err := store.SyncManagedConfig(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(past) {
		t.Fatalf("unchanged config was rewritten: mtime=%s", info.ModTime())
	}
}

func TestIsolatedConfigUsesRealTOMLForArraysAndQuotedProjects(t *testing.T) {
	primary := []byte(`features = [
  "one",
  "two",
]

[[tool.rules]]
name = "managed"

[projects."/primary"]
trust_level = "trusted"
`)
	local := []byte(`[projects."/secondary.with.dots"]
trust_level = "trusted"
`)
	contents, err := isolatedConfigContents(primary, local)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := toml.Unmarshal(contents, &decoded); err != nil {
		t.Fatalf("generated config is invalid TOML: %v\n%s", err, contents)
	}
	projects, ok := decoded["projects"].(map[string]any)
	if !ok || projects["/secondary.with.dots"] == nil || projects["/primary"] != nil {
		t.Fatalf("project trust was not isolated: %#v", decoded["projects"])
	}
	features, ok := decoded["features"].([]any)
	if !ok || len(features) != 2 {
		t.Fatalf("multiline array was not preserved: %#v", decoded["features"])
	}
}
