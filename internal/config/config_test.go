package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMissingFileIsNotAnError(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.env"))
	if err != nil {
		t.Fatalf("a missing env file should be tolerated: %v", err)
	}
	if got := c.Get("core.prometheus_url"); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestLaterFileWins(t *testing.T) {
	dir := t.TempDir()
	dist := write(t, dir, ".env.dist", "AD_CORE_PROMETHEUS_URL=http://dist:9090\n")
	local := write(t, dir, ".env.local", "AD_CORE_PROMETHEUS_URL=http://local:9090\n")
	c, err := Load(dist, local)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Get("core.prometheus_url"); got != "http://local:9090" {
		t.Fatalf("got %q, want the .env.local value", got)
	}
	if got := c.Source("core.prometheus_url"); got != local {
		t.Fatalf("source = %q, want %q", got, local)
	}
}

func TestEnvironmentBeatsFiles(t *testing.T) {
	dir := t.TempDir()
	dist := write(t, dir, ".env.dist", "AD_CORE_PROMETHEUS_URL=http://dist:9090\n")
	t.Setenv("AD_CORE_PROMETHEUS_URL", "http://env:9090")
	c, err := Load(dist)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Get("core.prometheus_url"); got != "http://env:9090" {
		t.Fatalf("got %q, want the environment value", got)
	}
	if got := c.Source("core.prometheus_url"); got != string(SourceEnv) {
		t.Fatalf("source = %q, want %q", got, SourceEnv)
	}
}

func TestParsing(t *testing.T) {
	dir := t.TempDir()
	f := write(t, dir, ".env", `
# a comment
export AD_A=plain
AD_B = "quoted value"
AD_C='single'
AD_D=has=equals
`)
	c, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"a": "plain", "b": "quoted value", "c": "single", "d": "has=equals"} {
		if got := c.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestUnprefixedKeyIsRejected(t *testing.T) {
	// A typo like PROMETHEUS_URL= would otherwise be silently ignored, which
	// is a miserable thing to debug.
	dir := t.TempDir()
	f := write(t, dir, ".env", "PROMETHEUS_URL=http://x:9090\n")
	if _, err := Load(f); err == nil {
		t.Fatal("expected an error for a key without the AD_ prefix")
	}
}

func TestEnvNameMapping(t *testing.T) {
	if got := EnvName("system.fritzhome.metric_prefix"); got != "AD_SYSTEM_FRITZHOME_METRIC_PREFIX" {
		t.Fatalf("got %q", got)
	}
}

func TestUnsetSystemIsEnabled(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.Enabled("host") {
		t.Fatal("an unconfigured system should default to enabled")
	}
}

func TestSystemCanBeDisabled(t *testing.T) {
	t.Setenv("AD_SYSTEM_HOST_ENABLED", "0")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Enabled("host") {
		t.Fatal("should be disabled")
	}
}

func TestScopeIsolatesSystems(t *testing.T) {
	t.Setenv("AD_SYSTEM_HOST_SECRET", "from-host")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Scoped("fritzhome").Get("secret"); got != "" {
		t.Fatalf("fritzhome read host's key: %q", got)
	}
	if got := c.Scoped("host").Get("secret"); got != "from-host" {
		t.Fatalf("got %q", got)
	}
}

func storeConfig(t *testing.T, files ...string) (*Config, string) {
	t.Helper()
	c, err := Load(files...)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "data", "settings.json")
	if err := c.UseStore(path); err != nil {
		t.Fatal(err)
	}
	return c, path
}

func TestStoreSitsBetweenDefaultsAndFiles(t *testing.T) {
	dir := t.TempDir()
	dist := write(t, dir, ".env.dist", "AD_CORE_PROMETHEUS_URL=http://dist:9090\nAD_SYSTEM_FRITZHOME_URL=http://dist\n")
	local := write(t, dir, ".env.local", "AD_SYSTEM_FRITZHOME_URL=http://local\nAD_SYSTEM_FRITZHOME_USERNAME=\n")
	c, _ := storeConfig(t, dist, local)

	if _, err := c.Set(map[string]string{"core.prometheus_url": "http://page:9090", "system.fritzhome.username": "me"}); err != nil {
		t.Fatal(err)
	}
	if got, src := c.Get("core.prometheus_url"), c.Source("core.prometheus_url"); got != "http://page:9090" || src != SourceSettings {
		t.Errorf("prometheus = %q from %q, want the page's value over .env.dist", got, src)
	}
	if got := c.Get("system.fritzhome.username"); got != "me" {
		t.Errorf("an empty line in a file locked the key: %q", got)
	}
	if _, err := c.Set(map[string]string{"system.fritzhome.url": "http://page"}); err == nil {
		t.Error("a key set in .env.local was changed from the page")
	}
	if got := c.Locked("system.fritzhome.url"); got != local {
		t.Errorf("locked by %q, want %q", got, local)
	}
}

func TestEmptyFileValueStillClearsDefault(t *testing.T) {
	dir := t.TempDir()
	dist := write(t, dir, ".env.dist", "AD_A=default\n")
	local := write(t, dir, ".env.local", "AD_A=\n")
	c, err := Load(dist, local)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Get("a"); got != "" {
		t.Errorf("got %q, want the default cleared", got)
	}
}

func TestBootstrapKeysAreNotStored(t *testing.T) {
	c, _ := storeConfig(t)
	for _, k := range Bootstrap {
		if _, err := c.Set(map[string]string{k: "x"}); err == nil {
			t.Errorf("%s was stored", k)
		}
	}
}

func TestLockedKeyRefusesTheWholeBatch(t *testing.T) {
	t.Setenv("AD_LINKS", "wiki")
	c, _ := storeConfig(t)
	if _, err := c.Set(map[string]string{"core.prometheus_url": "http://x", "links": "a"}); err == nil {
		t.Fatal("a key from the environment was changed")
	}
	if c.Has("core.prometheus_url") {
		t.Error("half the batch was applied")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	c, path := storeConfig(t)
	undo, err := c.Set(map[string]string{"core.prometheus_url": "http://p", "system.fritzhome.password": "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	again, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := again.UseStore(path); err != nil {
		t.Fatal(err)
	}
	if got := again.Get("system.fritzhome.password"); got != "s3cret" {
		t.Errorf("reloaded %q", got)
	}

	if _, err := c.Set(undo); err != nil {
		t.Fatal(err)
	}
	if c.Has("core.prometheus_url") {
		t.Error("undo left the value")
	}
}

// A crash between writing the temp file and renaming it leaves the old
// settings, and the stray temp file does not break the next load.
func TestInterruptedSaveKeepsTheOldFile(t *testing.T) {
	c, path := storeConfig(t)
	if _, err := c.Set(map[string]string{"core.prometheus_url": "http://old"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Dir(path), ".settings-123", `{"version":1,"values":{"AD_CORE_PROM`)
	again, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := again.UseStore(path); err != nil {
		t.Fatal(err)
	}
	if got := again.Get("core.prometheus_url"); got != "http://old" {
		t.Errorf("got %q", got)
	}
}

func TestUnknownStoredKeysAreKept(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "settings.json", `{"version":1,"values":{"AD_FUTURE_THING":"x"}}`)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.UseStore(path); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Set(map[string]string{"core.prometheus_url": "http://p"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "AD_FUTURE_THING") {
		t.Errorf("an unknown key was dropped: %s", b)
	}
}
