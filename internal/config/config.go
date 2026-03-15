package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// COSConfig holds COS credentials
type COSConfig struct {
	SecretID  string `json:"secret_id"`
	SecretKey string `json:"secret_key"`
	Bucket    string `json:"bucket"`
	Region    string `json:"region"`
}

// PerformanceConfig holds tuning parameters
type PerformanceConfig struct {
	DebounceMs      int     `json:"debounce_ms"`
	BatchIntervalMs int     `json:"batch_interval_ms"`
	BatchMaxSize    int     `json:"batch_max_size"`
	MaxConcurrent   int     `json:"max_concurrent"`
	QPS             float64 `json:"qps"`
}

// LogConfig holds log settings
type LogConfig struct {
	Level  string `json:"level"`
	Format string `json:"format"`
}

// Config is the root configuration structure stored in config.json
type Config struct {
	COS         COSConfig         `json:"cos"`
	Performance PerformanceConfig `json:"performance"`
	Log         LogConfig         `json:"log"`
}

// DefaultConfig returns a Config populated with sensible defaults
func DefaultConfig() *Config {
	return &Config{
		COS: COSConfig{
			Region: "ap-guangzhou",
		},
		Performance: PerformanceConfig{
			DebounceMs:      2000,
			BatchIntervalMs: 5000,
			BatchMaxSize:    100,
			MaxConcurrent:   3,
			QPS:             10,
		},
		Log: LogConfig{
			Level:  "info",
			Format: "json",
		},
	}
}

// Load reads config.json from path, applies defaults and env overrides
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("config file not found at %s — run 'cloudsync init' first", path)
		}
		return nil, fmt.Errorf("read config: %w", err)
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Environment variable overrides
	if v := os.Getenv("COS_SECRET_ID"); v != "" {
		cfg.COS.SecretID = v
	}
	if v := os.Getenv("COS_SECRET_KEY"); v != "" {
		cfg.COS.SecretKey = v
	}
	if v := os.Getenv("COS_BUCKET"); v != "" {
		cfg.COS.Bucket = v
	}
	if v := os.Getenv("COS_REGION"); v != "" {
		cfg.COS.Region = v
	}

	return cfg, nil
}

// Save writes cfg to path as JSON
func Save(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}
