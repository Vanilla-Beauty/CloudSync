package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/cloudsync/cloudsync/internal/config"
	"github.com/cloudsync/cloudsync/internal/limiter"
	"github.com/cloudsync/cloudsync/internal/storage"
	"github.com/cloudsync/cloudsync/internal/watcher"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	var cfgPath string

	root := &cobra.Command{
		Use:   "cloudsync",
		Short: "CloudSync — sync local folders to Tencent Cloud COS",
	}

	root.PersistentFlags().StringVar(&cfgPath, "config", "", "config file path (default: ~/.cloudsync.yaml)")

	root.AddCommand(initCmd(), startCmd(&cfgPath), statusCmd())
	return root
}

// initCmd generates default config and .syncignore in the current directory
func initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Generate default config and .syncignore in current directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit()
		},
	}
}

func runInit() error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	cfgDst := filepath.Join(cwd, "cloudsync.yaml")
	syncignoreDst := filepath.Join(cwd, ".syncignore")

	// Write default config
	defaultCfg := `# CloudSync configuration
sync:
  - name: "default"
    local_path: "."
    remote_prefix: "sync/"
    enabled: true

filter:
  ignore_file: ".syncignore"
  detect_swap: true

performance:
  debounce_ms: 2000
  batch_interval_ms: 5000
  batch_max_size: 100
  max_concurrent: 3
  qps: 10

# Set via environment variables: COS_SECRET_ID, COS_SECRET_KEY, COS_BUCKET, COS_REGION
cos:
  secret_id: ""
  secret_key: ""
  bucket: ""
  region: "ap-guangzhou"

log:
  level: "info"
  format: "json"
  output: "cloudsync.log"
`
	if err := os.WriteFile(cfgDst, []byte(defaultCfg), 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	fmt.Printf("Created %s\n", cfgDst)

	defaultIgnore := `# CloudSync ignore rules (gitignore syntax)
*.tmp
*.temp
*.bak
*.swp
*.swo
*.log
~$*
.#*
*~
node_modules/
dist/
build/
.git/
.DS_Store
Thumbs.db
`
	if err := os.WriteFile(syncignoreDst, []byte(defaultIgnore), 0644); err != nil {
		return fmt.Errorf("write .syncignore: %w", err)
	}
	fmt.Printf("Created %s\n", syncignoreDst)
	return nil
}

// startCmd starts the sync watcher
func startCmd(cfgPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start watching and syncing files",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStart(*cfgPath)
		},
	}
}

func runStart(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger, err := buildLogger(cfg)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer logger.Sync() //nolint:errcheck

	metadata := storage.NewMetadataStore()
	rl := limiter.NewRateLimiter(cfg.Performance.MaxConcurrent, cfg.Performance.QPS)

	cosClient, err := storage.NewCOSClient(&cfg.COS, metadata, logger)
	if err != nil {
		return fmt.Errorf("init COS client: %w", err)
	}

	var watchers []*watcher.SyncWatcher
	for _, task := range cfg.Sync {
		if !task.Enabled {
			continue
		}
		localPath, err := filepath.Abs(task.LocalPath)
		if err != nil {
			logger.Warn("invalid local_path", zap.String("name", task.Name), zap.Error(err))
			continue
		}

		sw, err := watcher.New(
			watcher.Config{
				LocalRoot:    localPath,
				RemotePrefix: task.RemotePrefix,
				IgnoreFile:   cfg.Filter.IgnoreFile,
				DetectSwap:   cfg.Filter.DetectSwap,
				Perf:         cfg.Performance,
			},
			cosClient,
			metadata,
			rl,
			logger,
		)
		if err != nil {
			logger.Error("create watcher failed", zap.String("task", task.Name), zap.Error(err))
			continue
		}

		if err := sw.Start(); err != nil {
			logger.Error("start watcher failed", zap.String("task", task.Name), zap.Error(err))
			continue
		}

		logger.Info("watching", zap.String("task", task.Name), zap.String("path", localPath))
		watchers = append(watchers, sw)
	}

	if len(watchers) == 0 {
		return fmt.Errorf("no sync tasks configured or all failed to start")
	}

	// Wait for signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	logger.Info("shutting down...")
	for _, sw := range watchers {
		sw.Stop()
	}
	logger.Info("stopped")
	return nil
}

// statusCmd prints a simple status message
func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show sync status",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("CloudSync running — uptime check at %s\n", time.Now().Format(time.RFC3339))
			fmt.Println("Use `cloudsync start` to begin syncing.")
		},
	}
}

func buildLogger(cfg *config.Config) (*zap.Logger, error) {
	level := zap.InfoLevel
	switch cfg.Log.Level {
	case "debug":
		level = zap.DebugLevel
	case "warn":
		level = zap.WarnLevel
	case "error":
		level = zap.ErrorLevel
	}

	encoderCfg := zap.NewProductionEncoderConfig()
	encoderCfg.TimeKey = "time"
	encoderCfg.EncodeTime = zapcore.ISO8601TimeEncoder

	var enc zapcore.Encoder
	if cfg.Log.Format == "console" {
		enc = zapcore.NewConsoleEncoder(encoderCfg)
	} else {
		enc = zapcore.NewJSONEncoder(encoderCfg)
	}

	var w zapcore.WriteSyncer
	if cfg.Log.Output == "" || cfg.Log.Output == "stdout" {
		w = zapcore.AddSync(os.Stdout)
	} else {
		f, err := os.OpenFile(cfg.Log.Output, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return nil, err
		}
		w = zapcore.AddSync(io.MultiWriter(os.Stdout, f))
	}

	core := zapcore.NewCore(enc, w, level)
	return zap.New(core), nil
}
