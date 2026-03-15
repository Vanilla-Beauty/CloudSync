package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudsync/cloudsync/internal/apiclient"
	"github.com/cloudsync/cloudsync/internal/config"
	"github.com/cloudsync/cloudsync/internal/daemon"
	"github.com/cloudsync/cloudsync/internal/filter"
	"github.com/cloudsync/cloudsync/internal/ipc"
	"github.com/cloudsync/cloudsync/internal/limiter"
	"github.com/cloudsync/cloudsync/internal/storage"
	"github.com/kardianos/service"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "cloudsync",
		Short: "CloudSync — sync local folders to Tencent Cloud COS",
	}
	root.AddCommand(
		initCmd(),
		startCmd(),
		stopCmd(),
		statusCmd(),
		mountCmd(),
		downloadCmd(),
		unmountCmd(),
		deleteCmd(),
		lsCmd(),
		pauseCmd(),
		resumeCmd(),
		syncCmd(),
	)
	return root
}

// ── init ──────────────────────────────────────────────────────────────────────

func initCmd() *cobra.Command {
	var (
		secretID  string
		secretKey string
		bucket    string
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Configure COS credentials and write config.json",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit(secretID, secretKey, bucket)
		},
	}
	cmd.Flags().StringVar(&secretID, "secret-id", "", "COS SecretId")
	cmd.Flags().StringVar(&secretKey, "secret-key", "", "COS SecretKey")
	cmd.Flags().StringVar(&bucket, "bucket", "", "Default COS bucket (skip interactive selection)")
	return cmd
}

func runInit(secretID, secretKey, bucket string) error {
	configDir, err := ipc.ConfigDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	reader := bufio.NewReader(os.Stdin)
	prompt := func(label, current string) string {
		if current != "" {
			return current
		}
		fmt.Printf("%s: ", label)
		v, _ := reader.ReadString('\n')
		return strings.TrimSpace(v)
	}

	secretID = prompt("COS SecretId", secretID)
	secretKey = prompt("COS SecretKey", secretKey)

	// Fetch available buckets and let the user choose one.
	var chosenBucket, chosenRegion string
	if bucket != "" {
		// User bypassed selection with --bucket; region is unknown, leave empty (daemon default applies).
		chosenBucket = bucket
	} else {
		fmt.Println("Fetching bucket list...")
		buckets, err := storage.ListBuckets(context.Background(), secretID, secretKey)
		if err != nil {
			return fmt.Errorf("list buckets: %w", err)
		}
		if len(buckets) == 0 {
			return fmt.Errorf("no buckets found for these credentials")
		}

		fmt.Println()
		for i, b := range buckets {
			fmt.Printf("  [%d] %s  (%s)  created %s\n", i+1, b.Name, b.Region, b.CreationDate)
		}
		fmt.Print("\nSelect default bucket [1]: ")
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)

		idx := 1
		if line != "" {
			if _, err := fmt.Sscanf(line, "%d", &idx); err != nil || idx < 1 || idx > len(buckets) {
				return fmt.Errorf("invalid selection %q", line)
			}
		}
		chosen := buckets[idx-1]
		chosenBucket = chosen.Name
		chosenRegion = chosen.Region
		fmt.Printf("Selected: %s (%s)\n", chosenBucket, chosenRegion)
	}

	cfg := config.DefaultConfig()
	cfg.COS.SecretID = secretID
	cfg.COS.SecretKey = secretKey
	cfg.COS.Bucket = chosenBucket
	cfg.COS.Region = chosenRegion

	cfgPath, _ := ipc.ConfigFilePath()
	if err := config.Save(cfgPath, cfg); err != nil {
		return err
	}
	fmt.Printf("Config written to %s\n", cfgPath)
	return nil
}

// ── start ─────────────────────────────────────────────────────────────────────

func startCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the cloudsyncd daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStart()
		},
	}
}

func runStart() error {
	socketPath, err := ipc.SocketPath()
	if err != nil {
		return err
	}
	client := apiclient.NewClient(socketPath)
	if client.Ping() == nil {
		fmt.Println("cloudsyncd is already running")
		return nil
	}

	// Find cloudsyncd binary in same directory as this binary
	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}
	daemonPath := filepath.Join(filepath.Dir(selfPath), "cloudsyncd")
	if _, err := os.Stat(daemonPath); os.IsNotExist(err) {
		return fmt.Errorf("cloudsyncd binary not found at %s", daemonPath)
	}

	svcCfg := daemon.BuildServiceConfig(daemonPath)
	svc, err := service.New(daemon.NewProgram(), svcCfg)
	if err != nil {
		return fmt.Errorf("service init: %w", err)
	}

	if err := svc.Install(); err != nil {
		// If already installed, continue to Start
		_ = err
	}
	if err := svc.Start(); err != nil {
		// Fall back to direct exec if service start fails (e.g., no root)
		proc := exec.Command(daemonPath)
		proc.Stdout = nil
		proc.Stderr = nil
		if startErr := proc.Start(); startErr != nil {
			return fmt.Errorf("start daemon: %w", startErr)
		}
		_ = proc.Process.Release()
	}

	// Poll until socket responds (up to 5s)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if client.Ping() == nil {
			fmt.Println("cloudsyncd started")
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("cloudsyncd did not start within 5 seconds")
}

// ── stop ──────────────────────────────────────────────────────────────────────

func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the cloudsyncd daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStop()
		},
	}
}

func runStop() error {
	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}
	daemonPath := filepath.Join(filepath.Dir(selfPath), "cloudsyncd")
	svcCfg := daemon.BuildServiceConfig(daemonPath)
	svc, err := service.New(daemon.NewProgram(), svcCfg)
	if err != nil {
		return fmt.Errorf("service init: %w", err)
	}
	if err := svc.Stop(); err != nil {
		return fmt.Errorf("stop daemon: %w", err)
	}
	fmt.Println("cloudsyncd stopped")
	return nil
}

// ── status ────────────────────────────────────────────────────────────────────

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show daemon status and active mounts",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus()
		},
	}
}

func runStatus() error {
	client, err := newClient()
	if err != nil {
		return err
	}
	st, err := client.Status()
	if err != nil {
		return err
	}
	fmt.Printf("Daemon PID:   %d\n", st.DaemonPID)
	fmt.Printf("Version:      %s\n", st.Version)
	fmt.Printf("Mounts:       %d\n", st.MountCount)

	mounts, err := client.ListMounts()
	if err != nil {
		return err
	}
	if len(mounts) == 0 {
		fmt.Println("\nNo active mounts.")
		return nil
	}
	fmt.Println()
	printMountsTable(mounts)
	return nil
}

// ── mount ─────────────────────────────────────────────────────────────────────

func mountCmd() *cobra.Command {
	var fromHome bool
	var bucket string
	cmd := &cobra.Command{
		Use:   "mount <path> [remote]",
		Short: "Start syncing a local directory",
		Long: `Start syncing a local directory to COS. Three modes:

  mount <path>               Remote prefix = basename of path  (e.g. /a/b/c → c/)
  mount --from-home <path>   Remote prefix = path relative to $HOME  (e.g. ~/a/b/c → a/b/c/)
  mount <path> <remote>      Remote prefix = explicitly specified value

Use --bucket to override the default bucket set during 'cloudsync init'.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			remote := ""
			if len(args) == 2 {
				remote = args[1]
			}
			return runMount(args[0], remote, bucket, fromHome)
		},
	}
	cmd.Flags().BoolVar(&fromHome, "from-home", false, "Use path relative to $HOME as remote prefix")
	cmd.Flags().StringVar(&bucket, "bucket", "", "COS bucket to use (overrides default)")
	return cmd
}

func runMount(path, remote, bucket string, fromHome bool) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	var prefix string
	switch {
	case remote != "":
		prefix = strings.TrimSuffix(remote, "/") + "/"
	case fromHome:
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("get home dir: %w", err)
		}
		rel, err := filepath.Rel(home, absPath)
		if err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("path %s is not under $HOME (%s)", absPath, home)
		}
		prefix = filepath.ToSlash(rel) + "/"
	default:
		prefix = filepath.Base(absPath) + "/"
	}

	client, err := newClient()
	if err != nil {
		return err
	}
	rec, err := client.AddMount(absPath, prefix, bucket, "")
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("Mounted: %s → %s (id: %s)", rec.LocalPath, rec.RemotePrefix, rec.ID)
	if rec.Bucket != "" {
		msg += fmt.Sprintf(" [bucket: %s]", rec.Bucket)
	}
	fmt.Println(msg)
	return nil
}

// ── download ──────────────────────────────────────────────────────────────────

func downloadCmd() *cobra.Command {
	var toHome bool
	var bucket string
	cmd := &cobra.Command{
		Use:   "download <remote> [local]",
		Short: "Download a remote COS prefix to a local directory and start bidirectional sync",
		Long: `Download a remote COS prefix to a local directory and establish a sync mount.
Three modes:

  download <remote>              Local path = current directory / basename(remote)
  download --to-home <remote>    Local path = $HOME/<remote>
  download <remote> <local>      Explicit local path

Use --bucket to override the default bucket set during 'cloudsync init'.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			local := ""
			if len(args) == 2 {
				local = args[1]
			}
			return runDownload(args[0], local, bucket, toHome)
		},
	}
	cmd.Flags().BoolVar(&toHome, "to-home", false, "Place the local directory under $HOME")
	cmd.Flags().StringVar(&bucket, "bucket", "", "COS bucket to use (overrides default)")
	return cmd
}

func runDownload(remote, local, bucket string, toHome bool) error {
	prefix := strings.TrimSuffix(remote, "/") + "/"

	var absLocal string
	switch {
	case local != "":
		var err error
		absLocal, err = filepath.Abs(local)
		if err != nil {
			return fmt.Errorf("resolve path: %w", err)
		}
	case toHome:
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("get home dir: %w", err)
		}
		absLocal = filepath.Join(home, filepath.FromSlash(strings.TrimSuffix(remote, "/")))
	default:
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("get working dir: %w", err)
		}
		absLocal = filepath.Join(cwd, filepath.Base(strings.TrimSuffix(remote, "/")))
	}

	if err := os.MkdirAll(absLocal, 0755); err != nil {
		return fmt.Errorf("create local dir: %w", err)
	}

	client, err := newClient()
	if err != nil {
		return err
	}
	rec, err := client.AddMount(absLocal, prefix, bucket, "")
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("Downloading: %s → %s (id: %s)", rec.RemotePrefix, rec.LocalPath, rec.ID)
	if rec.Bucket != "" {
		msg += fmt.Sprintf(" [bucket: %s]", rec.Bucket)
	}
	fmt.Println(msg)
	fmt.Println("Sync established. Remote files will be downloaded in the background.")
	return nil
}

// ── unmount ───────────────────────────────────────────────────────────────────

func unmountCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unmount <path>",
		Short: "Stop syncing a directory (remote files are preserved)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUnmount(args[0])
		},
	}
}

func runUnmount(path string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	client, err := newClient()
	if err != nil {
		return err
	}
	if err := client.RemoveMount(absPath, false); err != nil {
		return err
	}
	fmt.Printf("Unmounted: %s (remote files preserved)\n", absPath)
	return nil
}

// ── delete ────────────────────────────────────────────────────────────────────

func deleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <path>",
		Short: "Stop syncing and delete all remote files for this directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDelete(args[0])
		},
	}
}

func runDelete(path string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	fmt.Printf("This will delete all remote files for %s.\nContinue? [y/N] ", absPath)
	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	if strings.ToLower(strings.TrimSpace(answer)) != "y" {
		fmt.Println("Aborted.")
		return nil
	}

	client, err := newClient()
	if err != nil {
		return err
	}
	if err := client.RemoveMount(absPath, true); err != nil {
		return err
	}
	fmt.Printf("Deleted remote files and unmounted: %s\n", absPath)
	return nil
}

// ── ls ────────────────────────────────────────────────────────────────────────

func lsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List active mounts",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLs()
		},
	}
}

func runLs() error {
	client, err := newClient()
	if err != nil {
		return err
	}
	mounts, err := client.ListMounts()
	if err != nil {
		return err
	}
	if len(mounts) == 0 {
		fmt.Println("No active mounts.")
		return nil
	}
	printMountsTable(mounts)
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func newClient() (*apiclient.Client, error) {
	socketPath, err := ipc.SocketPath()
	if err != nil {
		return nil, err
	}
	return apiclient.NewClient(socketPath), nil
}

func printMountsTable(mounts []ipc.MountRecord) {
	fmt.Printf("%-10s  %-35s  %-20s  %-6s  %s\n", "ID", "LOCAL PATH", "REMOTE PREFIX", "STATUS", "LAST SYNC")
	fmt.Println(strings.Repeat("-", 95))
	for _, m := range mounts {
		status := "active"
		if m.Paused {
			status = "paused"
		}
		lastSync := "-"
		if m.LastSync != nil {
			lastSync = m.LastSync.Local().Format("2006-01-02 15:04:05")
		}
		fmt.Printf("%-10s  %-35s  %-20s  %-6s  %s\n",
			m.ID, m.LocalPath, m.RemotePrefix, status, lastSync)
		fmt.Printf("%-10s  uploads=%-6d downloads=%-6d deletes=%-6d errors=%d\n",
			"", m.Uploads, m.Downloads, m.Deletes, m.Errors)
	}
}

// ── pause ─────────────────────────────────────────────────────────────────────

func pauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pause <path>",
		Short: "Pause syncing for a mounted directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			absPath, err := filepath.Abs(args[0])
			if err != nil {
				return fmt.Errorf("resolve path: %w", err)
			}
			client, err := newClient()
			if err != nil {
				return err
			}
			if err := client.PauseMount(absPath); err != nil {
				return err
			}
			fmt.Printf("Paused: %s\n", absPath)
			return nil
		},
	}
}

// ── resume ────────────────────────────────────────────────────────────────────

func resumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume <path>",
		Short: "Resume syncing for a paused mounted directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			absPath, err := filepath.Abs(args[0])
			if err != nil {
				return fmt.Errorf("resolve path: %w", err)
			}
			client, err := newClient()
			if err != nil {
				return err
			}
			if err := client.ResumeMount(absPath); err != nil {
				return err
			}
			fmt.Printf("Resumed: %s\n", absPath)
			return nil
		},
	}
}

// ── sync ──────────────────────────────────────────────────────────────────────

func syncCmd() *cobra.Command {
	var bucket string
	cmd := &cobra.Command{
		Use:   "sync <path> [remote]",
		Short: "Run a one-shot bidirectional sync without the daemon",
		Long: `Perform a single bidirectional sync of <path> to the COS remote prefix and exit.
The daemon does not need to be running. Config is read from config.json.

  sync <path>           Remote prefix = basename of path
  sync <path> <remote>  Remote prefix = explicitly specified value

Use --bucket to override the default bucket.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			remote := ""
			if len(args) == 2 {
				remote = args[1]
			}
			return runSync(args[0], remote, bucket)
		},
	}
	cmd.Flags().StringVar(&bucket, "bucket", "", "COS bucket to use (overrides default)")
	return cmd
}

func runSync(path, remote, bucket string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	var prefix string
	if remote != "" {
		prefix = strings.TrimSuffix(remote, "/") + "/"
	} else {
		prefix = filepath.Base(absPath) + "/"
	}

	cfgPath, _ := ipc.ConfigFilePath()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	cosCfg := cfg.COS
	if bucket != "" {
		cosCfg.Bucket = bucket
	}

	metadata := storage.NewMetadataStore()
	rl := limiter.NewRateLimiter(cfg.Performance.MaxConcurrent, cfg.Performance.QPS)

	cosClient, err := storage.NewCOSClient(&cosCfg, metadata, zap.NewNop())
	if err != nil {
		return fmt.Errorf("init COS client: %w", err)
	}

	syncer := storage.NewSyncer(cosClient, metadata, rl, zap.NewNop(), absPath, prefix)

	ignorePath := filepath.Join(absPath, ".syncignore")
	ignoreRules, _ := filter.LoadIgnoreRules(ignorePath)
	swapDetector := filter.NewSwapDetector()
	syncer.SetIgnoreFunc(func(p string) bool {
		rel, err := filepath.Rel(absPath, p)
		if err != nil {
			rel = p
		}
		return ignoreRules.Match(rel) || swapDetector.IsSwapFile(p)
	})

	fmt.Printf("Syncing %s ↔ %s/%s ...\n", absPath, cosCfg.Bucket, prefix)
	if err := syncer.BidirSyncDirectory(context.Background()); err != nil {
		return fmt.Errorf("sync failed: %w", err)
	}

	stats := syncer.Stats()
	fmt.Printf("Done. uploads=%d  downloads=%d  deletes=%d  errors=%d\n",
		stats.Uploads, stats.Downloads, stats.Deletes, stats.Errors)
	return nil
}
