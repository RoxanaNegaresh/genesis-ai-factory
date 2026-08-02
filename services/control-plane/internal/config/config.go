// Package config is the only place in the program that reads the environment.
// Everything else receives a validated, immutable Config.
package config

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	Env      string
	Addr     string
	LogLevel string
	LogJSON  bool

	DBDriver string
	DBDSN    string

	RedisURL string

	JWTSecret       string
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration

	DataDir    string
	SingleUser bool

	CORSOrigins []string

	AIEngineAddr string

	// LLMBaseURL points at an OpenAI-compatible inference server. Empty means
	// no inference: the factory still runs, producing blueprint-derived output.
	LLMBaseURL string
	LLMAPIKey  string
	LLMModel   string
	// LLMTimeout bounds a single generation. CPU inference is slow, so this is
	// generous; a stuck request is caught by the per-agent budget instead.
	LLMTimeout time.Duration

	MaxParallelAgents int
	AutoMigrate       bool

	ShutdownGrace time.Duration
	RequestLimit  int

	Version   string
	Commit    string
	BuildDate string
}

// Defaults returns a configuration that boots with zero environment variables:
// SQLite in the user's data directory, loopback bind, dev secret. This matters
// because the desktop app must start the server without any setup step.
func Defaults() Config {
	return Config{
		Env:               "development",
		Addr:              "127.0.0.1:8787",
		LogLevel:          "info",
		LogJSON:           false,
		DBDriver:          "sqlite",
		JWTSecret:         "",
		AccessTokenTTL:    15 * time.Minute,
		RefreshTokenTTL:   30 * 24 * time.Hour,
		SingleUser:        true,
		CORSOrigins:       []string{"http://localhost:1420", "http://127.0.0.1:1420", "tauri://localhost"},
		AIEngineAddr:      "127.0.0.1:8790",
		MaxParallelAgents: 4,
		AutoMigrate:       true,
		ShutdownGrace:     20 * time.Second,
		RequestLimit:      4 << 20,
		Version:           "1.2.0",
	}
}

// Load builds the configuration from defaults, an optional .env file, and the
// process environment (in increasing order of precedence).
func Load() (Config, error) {
	c := Defaults()

	loadDotEnv(".env")

	c.Env = env("GENESIS_ENV", c.Env)
	c.Addr = env("GENESIS_ADDR", c.Addr)
	c.LogLevel = env("GENESIS_LOG_LEVEL", c.LogLevel)
	c.LogJSON = envBool("GENESIS_LOG_JSON", c.Env == "production")

	c.DataDir = env("GENESIS_DATA_DIR", defaultDataDir())
	if err := os.MkdirAll(c.DataDir, 0o750); err != nil {
		return c, fmt.Errorf("create data dir %s: %w", c.DataDir, err)
	}

	c.DBDriver = strings.ToLower(env("GENESIS_DB_DRIVER", c.DBDriver))
	c.DBDSN = env("GENESIS_DB_DSN", "")
	if c.DBDSN == "" {
		switch c.DBDriver {
		case "sqlite":
			// WAL keeps readers from blocking the writer, which matters because
			// the event stream reads while runs write continuously.
			c.DBDSN = "file:" + filepath.Join(c.DataDir, "genesis.db") +
				"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
		case "postgres":
			return c, fmt.Errorf("GENESIS_DB_DSN is required when GENESIS_DB_DRIVER=postgres")
		}
	}
	if c.DBDriver != "sqlite" && c.DBDriver != "postgres" {
		return c, fmt.Errorf("unsupported GENESIS_DB_DRIVER %q (want sqlite or postgres)", c.DBDriver)
	}

	c.RedisURL = env("GENESIS_REDIS_URL", "")
	c.AIEngineAddr = env("GENESIS_AI_ENGINE_ADDR", c.AIEngineAddr)

	c.LLMBaseURL = strings.TrimRight(env("GENESIS_LLM_URL", ""), "/")
	c.LLMAPIKey = env("GENESIS_LLM_API_KEY", "")
	c.LLMModel = env("GENESIS_LLM_MODEL", "")
	c.LLMTimeout = envDuration("GENESIS_LLM_TIMEOUT", 10*time.Minute)
	if c.LLMBaseURL != "" && !strings.HasSuffix(c.LLMBaseURL, "/v1") {
		// llama.cpp, vLLM and Ollama all expose the OpenAI routes under /v1.
		// Appending it removes the single most common configuration mistake.
		c.LLMBaseURL += "/v1"
	}

	c.JWTSecret = env("GENESIS_JWT_SECRET", "")
	if c.JWTSecret == "" {
		if c.Env == "production" {
			return c, fmt.Errorf("GENESIS_JWT_SECRET is required in production")
		}
		// Persist a generated secret so tokens survive a dev restart; a rotating
		// secret would log the user out on every rebuild.
		secret, err := persistedDevSecret(filepath.Join(c.DataDir, "jwt.secret"))
		if err != nil {
			return c, err
		}
		c.JWTSecret = secret
	}
	if len(c.JWTSecret) < 32 {
		return c, fmt.Errorf("GENESIS_JWT_SECRET must be at least 32 characters")
	}

	c.AccessTokenTTL = envDuration("GENESIS_ACCESS_TTL", c.AccessTokenTTL)
	c.RefreshTokenTTL = envDuration("GENESIS_REFRESH_TTL", c.RefreshTokenTTL)
	c.SingleUser = envBool("GENESIS_SINGLE_USER", c.SingleUser)
	c.AutoMigrate = envBool("GENESIS_AUTO_MIGRATE", c.AutoMigrate)
	c.MaxParallelAgents = envInt("FACTORY_MAX_PARALLEL_AGENTS", c.MaxParallelAgents)
	c.ShutdownGrace = envDuration("GENESIS_SHUTDOWN_GRACE", c.ShutdownGrace)
	c.RequestLimit = envInt("GENESIS_REQUEST_LIMIT", c.RequestLimit)

	if v := env("GENESIS_CORS_ORIGINS", ""); v != "" {
		c.CORSOrigins = splitAndTrim(v)
	}

	if c.MaxParallelAgents < 1 || c.MaxParallelAgents > 64 {
		return c, fmt.Errorf("FACTORY_MAX_PARALLEL_AGENTS must be between 1 and 64")
	}

	return c, nil
}

// WorkspaceRoot is where generated projects are written.
func (c Config) WorkspaceRoot() string { return filepath.Join(c.DataDir, "workspaces") }

// IsProduction reports whether production-grade guards apply.
func (c Config) IsProduction() bool { return c.Env == "production" }

// Redacted renders the config for logging with secrets removed.
func (c Config) Redacted() map[string]any {
	return map[string]any{
		"env": c.Env, "addr": c.Addr, "db_driver": c.DBDriver,
		"data_dir": c.DataDir, "single_user": c.SingleUser,
		"redis": c.RedisURL != "", "ai_engine": c.AIEngineAddr,
		"llm": c.LLMBaseURL != "", "llm_url": c.LLMBaseURL,
		"max_parallel_agents": c.MaxParallelAgents, "version": c.Version,
	}
}

func defaultDataDir() string {
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "genesis")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "genesis")
	}
	return filepath.Join(home, ".genesis")
}

func persistedDevSecret(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return strings.TrimSpace(string(b)), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate dev secret: %w", err)
	}
	secret := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
		return "", fmt.Errorf("persist dev secret: %w", err)
	}
	return secret, nil
}

// loadDotEnv reads KEY=VALUE lines without overriding real environment
// variables, which keeps container configuration authoritative.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return b
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}

func envDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return d
}

func splitAndTrim(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
