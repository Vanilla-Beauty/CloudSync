package config

import (
	"strings"

	"github.com/spf13/viper"
)

// SyncTask represents a single sync configuration
type SyncTask struct {
	Name         string `mapstructure:"name"`
	LocalPath    string `mapstructure:"local_path"`
	RemotePrefix string `mapstructure:"remote_prefix"`
	Enabled      bool   `mapstructure:"enabled"`
}

// FilterConfig holds filter settings
type FilterConfig struct {
	IgnoreFile  string `mapstructure:"ignore_file"`
	DetectSwap  bool   `mapstructure:"detect_swap"`
}

// PerformanceConfig holds tuning parameters
type PerformanceConfig struct {
	DebounceMs      int     `mapstructure:"debounce_ms"`
	BatchIntervalMs int     `mapstructure:"batch_interval_ms"`
	BatchMaxSize    int     `mapstructure:"batch_max_size"`
	MaxConcurrent   int     `mapstructure:"max_concurrent"`
	QPS             float64 `mapstructure:"qps"`
}

// COSConfig holds COS credentials
type COSConfig struct {
	SecretID  string `mapstructure:"secret_id"`
	SecretKey string `mapstructure:"secret_key"`
	Bucket    string `mapstructure:"bucket"`
	Region    string `mapstructure:"region"`
}

// LogConfig holds log settings
type LogConfig struct {
	Level  string `mapstructure:"level"`
	Format string `mapstructure:"format"`
	Output string `mapstructure:"output"`
}

// Config is the root configuration structure
type Config struct {
	Sync        []SyncTask        `mapstructure:"sync"`
	Filter      FilterConfig      `mapstructure:"filter"`
	Performance PerformanceConfig `mapstructure:"performance"`
	COS         COSConfig         `mapstructure:"cos"`
	Log         LogConfig         `mapstructure:"log"`
}

// Load reads config from the given path (or defaults) and applies env overrides
func Load(path string) (*Config, error) {
	v := viper.New()

	// Defaults
	v.SetDefault("filter.ignore_file", ".syncignore")
	v.SetDefault("filter.detect_swap", true)
	v.SetDefault("performance.debounce_ms", 2000)
	v.SetDefault("performance.batch_interval_ms", 5000)
	v.SetDefault("performance.batch_max_size", 100)
	v.SetDefault("performance.max_concurrent", 3)
	v.SetDefault("performance.qps", 10.0)
	v.SetDefault("cos.region", "ap-guangzhou")
	v.SetDefault("log.level", "info")
	v.SetDefault("log.format", "json")
	v.SetDefault("log.output", "cloudsync.log")

	if path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			return nil, err
		}
	}

	// Environment variable bindings (must be before Unmarshal)
	v.SetEnvPrefix("")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	_ = v.BindEnv("cos.secret_id", "COS_SECRET_ID")
	_ = v.BindEnv("cos.secret_key", "COS_SECRET_KEY")
	_ = v.BindEnv("cos.bucket", "COS_BUCKET")
	_ = v.BindEnv("cos.region", "COS_REGION")

	cfg := &Config{}
	if err := v.Unmarshal(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}
