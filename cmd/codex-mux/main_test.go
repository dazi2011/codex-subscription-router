package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInteractiveAppServerDetection(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{args: []string{"-c", "features.code_mode_host=true", "app-server", "--analytics-default-enabled"}, want: true},
		{args: []string{"app-server", "daemon", "version"}, want: false},
		{args: []string{"app-server", "generate-ts", "--out", "/tmp/schema"}, want: false},
		{args: []string{"exec", "hello"}, want: false},
	}
	for _, test := range tests {
		if got := isInteractiveAppServer(test.args); got != test.want {
			t.Fatalf("isInteractiveAppServer(%q)=%v, want %v", test.args, got, test.want)
		}
	}
}

func TestValidateControlToken(t *testing.T) {
	valid := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if got, err := validateControlToken("\n" + valid + "\t"); err != nil || got != valid {
		t.Fatalf("validateControlToken(valid) = %q, %v", got, err)
	}
	for _, invalid := range []string{"short", valid + "00", valid[:63] + "z"} {
		if _, err := validateControlToken(invalid); err == nil {
			t.Fatalf("validateControlToken(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestLoadControlTokenRequiresBuildToken(t *testing.T) {
	root := t.TempDir()
	if _, err := loadOrCreateToken(root); err == nil || !strings.Contains(err.Error(), "rebuild the app") {
		t.Fatalf("missing token error = %v", err)
	}
	valid := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(filepath.Join(root, "control-token"), []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := loadOrCreateToken(root); err != nil || got != valid {
		t.Fatalf("loadOrCreateToken() = %q, %v", got, err)
	}
}

func TestLoadControlTokenRejectsRuntimeOverride(t *testing.T) {
	t.Setenv("CODEX_MUX_CONTROL_TOKEN", strings.Repeat("a", 64))
	if _, err := loadOrCreateToken(t.TempDir()); err == nil {
		t.Fatal("runtime token override unexpectedly succeeded")
	}
}
