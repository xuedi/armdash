// Package config loads configuration from env files, the environment and the
// settings the owner saves on the settings page.
//
// Sources, from weakest to strongest:
//
//	.env.dist    committed defaults
//	settings     saved on the settings page, settings.json in the data directory
//	other files  .env.local, /etc/armdash/armdash.env
//	environment  wins over all, so containers and systemd need no files
//
// A file the operator wrote wins over the page, so an existing install and a
// container keep working unchanged, and a value set there shows on the page
// as locked. Moving it to the page is deleting its line. A few bootstrap keys,
// where to listen and where the settings live, are never taken from the page.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Prefix keeps our variables out of the way of everything else in the
// environment.
const Prefix = "AD_"

const (
	SourceDist     = ".env.dist"
	SourceEnv      = "environment"
	SourceSettings = "settings"
)

// Bootstrap keys decide where the process listens and where the settings are
// stored, so they are read before any page exists and never from the store.
var Bootstrap = []string{"core.addr", "core.tls_addr", "core.tls_cert", "core.tls_key", "core.data_dir"}

type value struct {
	Value  string
	Source string
}

type Config struct {
	mu sync.RWMutex
	// dist holds the committed defaults, user every file the operator wrote
	// and the environment. An empty value in user still clears a default.
	dist, user map[string]value
	store      *Store
}

// Load reads the given env files in order, then overlays the process
// environment. A missing file is not an error: a deployment may configure
// everything through real environment variables. A file named .env.dist holds
// defaults, which the settings page may override; any other file wins over it.
func Load(files ...string) (*Config, error) {
	c := &Config{dist: map[string]value{}, user: map[string]value{}}
	for _, f := range files {
		layer := c.user
		if filepath.Base(f) == SourceDist {
			layer = c.dist
		}
		if err := loadFile(f, layer); err != nil {
			return nil, err
		}
	}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(k, Prefix) {
			continue
		}
		c.user[k] = value{Value: v, Source: SourceEnv}
	}
	return c, nil
}

func loadFile(path string, into map[string]value) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")
		k, v, ok := strings.Cut(text, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected KEY=value", path, line)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		// Quotes are stripped so a value may contain spaces or a leading #.
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if !strings.HasPrefix(k, Prefix) {
			return fmt.Errorf("%s:%d: %q must start with %s", path, line, k, Prefix)
		}
		into[k] = value{Value: v, Source: path}
	}
	return sc.Err()
}

// envName turns a dotted key into its environment variable name:
// system.fritzhome.metric_prefix becomes AD_SYSTEM_FRITZHOME_METRIC_PREFIX.
func envName(key string) string {
	return Prefix + strings.ToUpper(strings.NewReplacer(".", "_", "-", "_").Replace(key))
}

// EnvName exposes the variable name a dotted key maps to, so the settings page
// can tell the reader exactly what to put in a file.
func EnvName(key string) string { return envName(key) }

// lookup resolves one variable name. The caller holds mu.
func (c *Config) lookup(name string) (value, bool) {
	if v, ok := c.user[name]; ok && v.Value != "" {
		return v, true
	}
	if c.store != nil {
		if v, ok := c.store.values[name]; ok && v != "" {
			return value{Value: v, Source: SourceSettings}, true
		}
	}
	if v, ok := c.user[name]; ok {
		return v, true
	}
	v, ok := c.dist[name]
	return v, ok
}

func (c *Config) Get(key string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, _ := c.lookup(envName(key))
	return v.Value
}

func (c *Config) GetOr(key, def string) string {
	if v := c.Get(key); v != "" {
		return v
	}
	return def
}

func (c *Config) Bool(key string) bool { return truthy(c.Get(key)) }

func truthy(s string) bool {
	switch strings.ToLower(s) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Source reports which file or environment a key came from, for the settings
// page. Knowing a value came from .env.dist rather than .env.local is usually
// the answer when something is unexpectedly empty.
func (c *Config) Source(key string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.lookup(envName(key))
	if !ok {
		return ""
	}
	return v.Source
}

// Has reports whether a key has a value at all.
func (c *Config) Has(key string) bool { return c.Get(key) != "" }

// Keys returns every set variable name, sorted.
func (c *Config) Keys() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	seen := map[string]bool{}
	for _, m := range []map[string]value{c.dist, c.user} {
		for k, v := range m {
			if v.Value != "" {
				seen[k] = true
			}
		}
	}
	if c.store != nil {
		for k, v := range c.store.values {
			if v != "" {
				seen[k] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Enabled reports whether a system should be shown. Unset means enabled, so a
// fresh checkout shows everything rather than an empty shell.
func (c *Config) Enabled(systemID string) bool {
	key := "system." + systemID + ".enabled"
	if !c.Has(key) {
		return true
	}
	return c.Bool(key)
}

// Writable reports whether there is a store to save settings into.
func (c *Config) Writable() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.store != nil
}

// Locked returns the file or "environment" that fixes a key, or "" when the
// page may change it. A bootstrap key is always locked.
func (c *Config) Locked(key string) string { return c.LockedVar(envName(key)) }

// LockedVar is Locked for a variable name.
func (c *Config) LockedVar(name string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if v, ok := c.user[name]; ok && v.Value != "" {
		return v.Source
	}
	if isBootstrap(name) {
		return "bootstrap"
	}
	return ""
}

func isBootstrap(name string) bool {
	for _, k := range Bootstrap {
		if envName(k) == name {
			return true
		}
	}
	return false
}

// Set changes stored values in memory, "" removing one, and returns what
// undoes it. Nothing is written until Save: the caller first checks that the
// new configuration builds. Every key must be writable, or nothing changes.
func (c *Config) Set(changes map[string]string) (undo map[string]string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.store == nil {
		return nil, fmt.Errorf("no data directory, settings cannot be saved")
	}
	for key := range changes {
		name := envName(key)
		if isBootstrap(name) {
			return nil, fmt.Errorf("%s is only read from the env file", name)
		}
		if v, ok := c.user[name]; ok && v.Value != "" {
			return nil, fmt.Errorf("%s is set in %s, which wins over the page", name, v.Source)
		}
	}
	undo = make(map[string]string, len(changes))
	for key, v := range changes {
		name := envName(key)
		undo[key] = c.store.values[name]
		if v == "" {
			delete(c.store.values, name)
		} else {
			c.store.values[name] = v
		}
	}
	return undo, nil
}

// Save writes the stored values to disk.
func (c *Config) Save() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.store == nil {
		return fmt.Errorf("no data directory, settings cannot be saved")
	}
	return c.store.save()
}

// UseStore loads the settings saved on the page from path, which need not
// exist yet.
func (c *Config) UseStore(path string) error {
	s, err := openStore(path)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.store = s
	c.mu.Unlock()
	return nil
}

// StorePath is where saved settings live, or "" without a data directory.
func (c *Config) StorePath() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.store == nil {
		return ""
	}
	return c.store.path
}

// Scoped restricts a system to its own namespace so it cannot read another
// system's credentials by accident.
func (c *Config) Scoped(systemID string) *Scope {
	return &Scope{c: c, prefix: "system." + systemID + "."}
}

type Scope struct {
	c      *Config
	prefix string
}

func (s *Scope) Get(key string) string      { return s.c.Get(s.prefix + key) }
func (s *Scope) GetOr(k, def string) string { return s.c.GetOr(s.prefix+k, def) }
func (s *Scope) Bool(key string) bool       { return s.c.Bool(s.prefix + key) }
func (s *Scope) Source(key string) string   { return s.c.Source(s.prefix + key) }
func (s *Scope) Locked(key string) string   { return s.c.Locked(s.prefix + key) }
func (s *Scope) EnvName(key string) string  { return envName(s.prefix + key) }
