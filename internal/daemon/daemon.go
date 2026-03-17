package daemon

import (
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/cloudsync/cloudsync/internal/apiserver"
	"github.com/cloudsync/cloudsync/internal/config"
	"github.com/cloudsync/cloudsync/internal/ipc"
	"github.com/cloudsync/cloudsync/internal/limiter"
	"github.com/cloudsync/cloudsync/internal/storage"
	"github.com/kardianos/service"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	ServiceName        = "cloudsyncd"
	ServiceDisplayName = "CloudSync Daemon"
	ServiceDescription = "CloudSync file synchronization daemon"
)

// Program implements service.Interface for kardianos/service.
type Program struct {
	logger       *zap.Logger
	mountManager *MountManager
	apiServer    *apiserver.Server
	stopCh       chan struct{}
	version      string
}

// BuildServiceConfig returns the kardianos/service config.
func BuildServiceConfig(execPath string) *service.Config {
	return &service.Config{
		Name:        ServiceName,
		DisplayName: ServiceDisplayName,
		Description: ServiceDescription,
		Executable:  execPath,
	}
}

// NewProgram creates the Program struct (without starting).
func NewProgram(version string) *Program {
	return &Program{
		stopCh:  make(chan struct{}),
		version: version,
	}
}

// Start is called by kardianos/service when the daemon is started.
func (p *Program) Start(s service.Service) error {
	go p.run()
	return nil
}

// Stop is called by kardianos/service when the daemon is stopped.
func (p *Program) Stop(s service.Service) error {
	close(p.stopCh)

	if p.mountManager != nil {
		p.mountManager.StopAll()
	}
	if p.apiServer != nil {
		p.apiServer.Stop()
	}
	if p.logger != nil {
		p.logger.Info("cloudsyncd stopped")
		_ = p.logger.Sync()
	}
	return nil
}

func (p *Program) run() {
	configDir, err := ipc.ConfigDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cloudsyncd: config dir error: %v\n", err)
		return
	}

	if err := os.MkdirAll(configDir, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "cloudsyncd: mkdir error: %v\n", err)
		return
	}

	cfgPath, _ := ipc.ConfigFilePath()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cloudsyncd: load config: %v\n", err)
		return
	}

	logger, err := buildDaemonLogger(cfg, configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cloudsyncd: init logger: %v\n", err)
		return
	}
	p.logger = logger

	metadata := storage.NewMetadataStore()
	rl := limiter.NewRateLimiter(cfg.Performance.MaxConcurrent, cfg.Performance.QPS)

	cosClient, err := storage.NewCOSClient(&cfg.COS, metadata, logger)
	if err != nil {
		logger.Error("init COS client failed", zap.Error(err))
		return
	}

	mountsPath, _ := ipc.MountsFilePath()
	p.mountManager = NewMountManager(mountsPath, cosClient, metadata, rl, cfg, logger)

	if err := p.mountManager.LoadSaved(); err != nil {
		logger.Warn("load saved mounts failed", zap.Error(err))
	}

	socketPath, _ := ipc.SocketPath()
	p.apiServer = apiserver.NewServer(p.mountManager, logger, p.version)
	if err := p.apiServer.Start(socketPath); err != nil {
		logger.Error("start API server failed", zap.Error(err))
		return
	}

	// Write PID file
	pidPath, _ := ipc.PIDFilePath()
	_ = os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0600)

	logger.Info("cloudsyncd started", zap.Int("pid", os.Getpid()), zap.String("socket", socketPath))

	// Wait for stop signal
	<-p.stopCh

	_ = os.Remove(pidPath)
}

func buildDaemonLogger(cfg *config.Config, configDir string) (*zap.Logger, error) {
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

	logPath := configDir + "/cloudsyncd.log"
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	w := zapcore.AddSync(io.MultiWriter(os.Stdout, f))

	core := zapcore.NewCore(enc, w, level)
	return zap.New(core), nil
}
