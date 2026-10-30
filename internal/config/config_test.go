package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFromEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.env")
	err := os.WriteFile(path, []byte(`
# comment
DATABASE_URL=postgres://from-file
LEGION_JWT_SECRET=0123456789abcdef0123456789abcdef
LEGION_ACCESS_TTL=5m
LEGION_ADDR=:9000
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEGION_ENV_FILE", path)
	t.Setenv("LEGION_ADDR", ":7000") // real env wins over the file
	for _, k := range []string{"DATABASE_URL", "LEGION_JWT_SECRET", "LEGION_ACCESS_TTL"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}

	if err := LoadEnvFile(); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.DatabaseURL != "postgres://from-file" || c.AccessTTL.Minutes() != 5 || c.Addr != ":7000" {
		t.Fatalf("got %+v", c)
	}
}

func TestLoadEnvFileMissing(t *testing.T) {
	t.Chdir(t.TempDir())
	os.Unsetenv("LEGION_ENV_FILE")
	if err := LoadEnvFile(); err != nil {
		t.Fatalf("missing default .env should be ignored: %v", err)
	}
	t.Setenv("LEGION_ENV_FILE", "/nonexistent/.env")
	if err := LoadEnvFile(); err == nil {
		t.Fatal("missing explicit env file should fail")
	}
}

func TestLoadRequiresSecret(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("LEGION_JWT_SECRET", "short")
	if _, err := Load(); err == nil {
		t.Fatal("short secret accepted")
	}
}
