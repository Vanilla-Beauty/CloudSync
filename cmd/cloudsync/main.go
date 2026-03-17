package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cloudsync/cloudsync/internal/apiclient"
	"github.com/cloudsync/cloudsync/internal/config"
	"github.com/cloudsync/cloudsync/internal/daemon"
	"github.com/cloudsync/cloudsync/internal/ipc"
	"github.com/cloudsync/cloudsync/internal/storage"
	"github.com/cloudsync/cloudsync/pkg/utils"
	"github.com/kardianos/service"
	"github.com/spf13/cobra"
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
		pullCmd(),
		unmountCmd(),
		deleteCmd(),
		lsCmd(),
		lsRemoteCmd(),
		lsBucketCmd(),
		lsBucketRemoteCmd(),
		versionCmd(),
	)
	return root
}

// ── version ───────────────────────────────────────────────────────────────────

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version, build time, and Go runtime version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("cloudsync  %s\n", version)
			fmt.Printf("built      %s\n", buildTime)
			fmt.Printf("go         %s\n", runtimeGoVersion())
		},
	}
}

// ── init ──────────────────────────────────────────────────────────────────────

func initCmd() *cobra.Command {
	var (
		secretID  string
		secretKey string
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Configure COS credentials and write config.json",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit(secretID, secretKey)
		},
	}
	cmd.Flags().StringVar(&secretID, "secret-id", "", "COS SecretId")
	cmd.Flags().StringVar(&secretKey, "secret-key", "", "COS SecretKey")
	return cmd
}

func runInit(secretID, secretKey string) error {
	configDir, err := ipc.ConfigDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	reader := bufio.NewReader(os.Stdin)

	promptLine := func(label, current string) string {
		if current != "" {
			return current
		}
		fmt.Printf("%s: ", label)
		v, _ := reader.ReadString('\n')
		return strings.TrimSpace(v)
	}

	secretID = promptLine("COS SecretId", secretID)
	secretKey = promptLine("COS SecretKey", secretKey)

	// Fetch bucket list with the provided credentials.
	fmt.Println("Fetching bucket list...")
	ctx := context.Background()
	buckets, err := storage.ListBuckets(ctx, secretID, secretKey)
	if err != nil {
		return fmt.Errorf("could not list buckets (check credentials): %w", err)
	}
	if len(buckets) == 0 {
		return fmt.Errorf("no buckets found for these credentials")
	}

	// Present numbered list for selection.
	fmt.Println("\nAvailable buckets:")
	for i, b := range buckets {
		fmt.Printf("  %d) %s  (%s)\n", i+1, b.Name, b.Region)
	}
	fmt.Printf("Select default bucket [1-%d]: ", len(buckets))
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	idx, err := strconv.Atoi(line)
	if err != nil || idx < 1 || idx > len(buckets) {
		return fmt.Errorf("invalid selection: %q", line)
	}
	chosen := buckets[idx-1]

	cfg := config.DefaultConfig()
	cfg.COS.SecretID = secretID
	cfg.COS.SecretKey = secretKey
	cfg.COS.Bucket = chosen.Name
	cfg.COS.Region = chosen.Region

	cfgPath, _ := ipc.ConfigFilePath()
	if err := config.Save(cfgPath, cfg); err != nil {
		return err
	}
	fmt.Printf("\nConfig written to %s\n", cfgPath)
	fmt.Printf("Default bucket: %s (%s)\n", chosen.Name, chosen.Region)
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

	// Find cloudsyncd binary in same directory as this binary.
	// On Windows the binary has a .exe suffix.
	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}
	daemonName := "cloudsyncd"
	if strings.HasSuffix(strings.ToLower(selfPath), ".exe") {
		daemonName = "cloudsyncd.exe"
	}
	daemonPath := filepath.Join(filepath.Dir(selfPath), daemonName)
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
	daemonName := "cloudsyncd"
	if strings.HasSuffix(strings.ToLower(selfPath), ".exe") {
		daemonName = "cloudsyncd.exe"
	}
	daemonPath := filepath.Join(filepath.Dir(selfPath), daemonName)
	svcCfg := daemon.BuildServiceConfig(daemonPath)
	svc, err := service.New(daemon.NewProgram(), svcCfg)
	if err != nil {
		return fmt.Errorf("service init: %w", err)
	}
	if err := svc.Stop(); err == nil {
		fmt.Println("cloudsyncd stopped")
		return nil
	}

	// Service manager failed (daemon was started directly, not via systemd/SCM).
	// Fall back to reading the PID file and terminating the process.
	pidPath, err := ipc.PIDFilePath()
	if err != nil {
		return fmt.Errorf("resolve pid file: %w", err)
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return fmt.Errorf("daemon is not running (pid file not found)")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return fmt.Errorf("invalid pid file: %w", err)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("process not found: %w", err)
	}
	if err := ipc.Terminate(proc); err != nil {
		return fmt.Errorf("terminate pid %d: %w", pid, err)
	}

	// Wait up to 5 s for daemon to stop (poll via ping).
	socketPath, _ := ipc.SocketPath()
	stopClient := apiclient.NewClient(socketPath)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if stopClient.Ping() != nil {
			break // daemon no longer responding
		}
		time.Sleep(200 * time.Millisecond)
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
  mount <path> <remote>      Remote prefix = explicitly specified value`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			remote := ""
			if len(args) == 2 {
				remote = args[1]
			}
			return runMount(args[0], remote, fromHome, bucket)
		},
	}
	cmd.Flags().BoolVar(&fromHome, "from-home", false, "Use path relative to $HOME as remote prefix")
	cmd.Flags().StringVar(&bucket, "bucket", "", "Override default COS bucket for this mount")
	return cmd
}

func runMount(path, remote string, fromHome bool, bucket string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	var prefix string
	switch {
	case remote != "":
		// Mode 3: explicit remote path
		prefix = utils.NormalizeRemotePrefix(remote)
	case fromHome:
		// Mode 2: relative to $HOME
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("get home dir: %w", err)
		}
		rel, err := filepath.Rel(home, absPath)
		if err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("path %s is not under $HOME (%s)", absPath, home)
		}
		prefix = utils.NormalizeRemotePrefix(rel)
	default:
		// Mode 1: basename only
		prefix = utils.NormalizeRemotePrefix(filepath.Base(absPath))
	}

	client, err := newClient()
	if err != nil {
		return err
	}
	rec, err := client.AddMount(absPath, prefix, false, bucket)
	if err != nil {
		return err
	}
	bucketDisplay := rec.Bucket
	if bucketDisplay == "" {
		bucketDisplay = "(default)"
	}
	fmt.Printf("Mounted: %s → %s  bucket: %s  (id: %s)\n",
		rec.LocalPath, rec.RemotePrefix, bucketDisplay, rec.ID)
	return nil
}

// ── pull ──────────────────────────────────────────────────────────────────────

func pullCmd() *cobra.Command {
	var bucket string
	cmd := &cobra.Command{
		Use:   "pull <remote-prefix> <local-path>",
		Short: "Download a COS prefix to a local directory and begin syncing",
		Long: `Download all files under a COS remote prefix to a local directory,
then establish bidirectional sync (local changes are uploaded automatically).

The local directory is created if it does not exist.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPull(args[0], args[1], bucket)
		},
	}
	cmd.Flags().StringVar(&bucket, "bucket", "", "Override default COS bucket for this pull")
	return cmd
}

func runPull(remotePrefix, path string, bucket string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	if err := os.MkdirAll(absPath, 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	prefix := utils.NormalizeRemotePrefix(remotePrefix)

	client, err := newClient()
	if err != nil {
		return err
	}
	rec, err := client.AddMount(absPath, prefix, true, bucket)
	if err != nil {
		return err
	}
	bucketDisplay := rec.Bucket
	if bucketDisplay == "" {
		bucketDisplay = "(default)"
	}
	fmt.Printf("Pulling %s → %s  bucket: %s  (id: %s) — downloading in background...\n",
		rec.RemotePrefix, rec.LocalPath, bucketDisplay, rec.ID)
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
		Short: "Interactively browse active local mounts (TUI)",
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

	b := newMountBrowser(client, mounts)
	for {
		rec, err := b.run()
		if err != nil {
			return err
		}
		if rec == nil {
			// User quit
			return nil
		}
		// User pressed 'r' — open remote browser for this mount.
		cfg, cfgErr := loadConfig()
		if cfgErr != nil {
			b.status = "Config error: " + cfgErr.Error()
			continue
		}
		bucket := rec.Bucket
		if bucket == "" {
			bucket = cfg.COS.Bucket
		}
		region := cfg.COS.Region
		if bucket != cfg.COS.Bucket {
			region, err = findBucketRegion(context.Background(), cfg.COS.SecretID, cfg.COS.SecretKey, bucket)
			if err != nil {
				b.status = "Region error: " + err.Error()
				continue
			}
		}
		cosClient, cosErr := storage.NewCOSClientForBucket(cfg.COS.SecretID, cfg.COS.SecretKey, bucket, region)
		if cosErr != nil {
			b.status = "COS error: " + cosErr.Error()
			continue
		}
		rb := newBrowser(cosClient, bucket, rec.RemotePrefix, client)
		if runErr := rb.run(); runErr != nil {
			b.status = "Browse error: " + runErr.Error()
		}
		// After remote browse closes, refresh mounts and return to mount browser
		newMounts, listErr := client.ListMounts()
		if listErr == nil {
			b.mounts = newMounts
			b.clampCursor()
		}
		b.status = ""
	}
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
	fmt.Printf("%-10s  %-35s  %-20s  %-30s  %s\n", "ID", "LOCAL PATH", "REMOTE PREFIX", "BUCKET", "ADDED AT")
	fmt.Println(strings.Repeat("-", 110))
	for _, m := range mounts {
		bucket := m.Bucket
		if bucket == "" {
			bucket = "(default)"
		}
		fmt.Printf("%-10s  %-35s  %-20s  %-30s  %s\n",
			m.ID, m.LocalPath, m.RemotePrefix, bucket, m.AddedAt.Local().Format(time.RFC3339))
	}
}

// ── ls-remote ─────────────────────────────────────────────────────────────────

func lsRemoteCmd() *cobra.Command {
	var (
		bucketFlag string
		prefix     string
	)
	cmd := &cobra.Command{
		Use:   "ls-remote",
		Short: "Interactively browse a COS bucket (lazygit-style)",
		Long: `Browse a COS bucket lazily: directories are expanded on demand.

  ↑↓ / j k   navigate
  Enter / l   expand directory
  ← / h       collapse directory / go to parent
  q / Esc     quit`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLsRemote(bucketFlag, prefix)
		},
	}
	cmd.Flags().StringVar(&bucketFlag, "bucket", "", "Bucket to browse (default: configured default bucket)")
	cmd.Flags().StringVar(&prefix, "prefix", "", "Start browsing from this prefix")
	return cmd
}

func runLsRemote(bucketOverride, prefix string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	bucket := cfg.COS.Bucket
	if bucketOverride != "" {
		bucket = bucketOverride
	}
	if bucket == "" {
		return fmt.Errorf("no bucket specified — use --bucket or run 'cloudsync init' to set a default")
	}

	// Find the region for this bucket: if it matches the default, use configured
	// region; otherwise query the service to find it.
	region := cfg.COS.Region
	if bucketOverride != "" && bucketOverride != cfg.COS.Bucket {
		region, err = findBucketRegion(context.Background(), cfg.COS.SecretID, cfg.COS.SecretKey, bucketOverride)
		if err != nil {
			return err
		}
	}

	cosClient, err := storage.NewCOSClientForBucket(cfg.COS.SecretID, cfg.COS.SecretKey, bucket, region)
	if err != nil {
		return err
	}

	// Best-effort: connect to daemon for interactive delete/sync support.
	// If the daemon is not running, apiClient is nil and those features are disabled.
	apiClient, apiErr := newClient()
	if apiErr != nil {
		apiClient = nil
	}

	b := newBrowser(cosClient, bucket, prefix, apiClient)
	return b.run()
}

// ── ls-bucket ─────────────────────────────────────────────────────────────────

func lsBucketCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls-bucket",
		Short: "Show configured default bucket and per-mount bucket overrides",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLsBucket()
		},
	}
}

func runLsBucket() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	fmt.Printf("Default bucket:  %s\n", cfg.COS.Bucket)
	fmt.Printf("Region:          %s\n", cfg.COS.Region)

	// Show per-mount bucket overrides (best-effort, daemon may not be running)
	apiClient, apiErr := newClient()
	if apiErr != nil {
		fmt.Println("\n(daemon not running — cannot show per-mount overrides)")
		return nil
	}
	mounts, err := apiClient.ListMounts()
	if err != nil {
		fmt.Println("\n(could not list mounts:", err, ")")
		return nil
	}

	// Collect only mounts that override the default bucket
	var overrides []ipc.MountRecord
	for _, m := range mounts {
		if m.Bucket != "" && m.Bucket != cfg.COS.Bucket {
			overrides = append(overrides, m)
		}
	}
	if len(overrides) == 0 {
		fmt.Println("\nNo per-mount bucket overrides.")
		return nil
	}

	fmt.Printf("\nPer-mount bucket overrides (%d):\n", len(overrides))
	fmt.Printf("%-10s  %-35s  %s\n", "ID", "LOCAL PATH", "BUCKET")
	fmt.Println(strings.Repeat("-", 70))
	for _, m := range overrides {
		fmt.Printf("%-10s  %-35s  %s\n", m.ID, m.LocalPath, m.Bucket)
	}
	return nil
}

// ── ls-bucket-remote ──────────────────────────────────────────────────────────

func lsBucketRemoteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls-bucket-remote",
		Short: "List all COS buckets accessible with the configured credentials",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLsBucketRemote()
		},
	}
}

func runLsBucketRemote() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	fmt.Println("Fetching bucket list...")
	buckets, err := storage.ListBuckets(context.Background(), cfg.COS.SecretID, cfg.COS.SecretKey)
	if err != nil {
		return fmt.Errorf("list buckets: %w", err)
	}
	if len(buckets) == 0 {
		fmt.Println("No buckets found.")
		return nil
	}

	defaultMark := func(name string) string {
		if name == cfg.COS.Bucket {
			return " ◀ default"
		}
		return ""
	}

	fmt.Printf("\n%-50s  %s\n", "BUCKET", "REGION")
	fmt.Println(strings.Repeat("-", 70))
	for _, b := range buckets {
		fmt.Printf("%-50s  %s%s\n", b.Name, b.Region, defaultMark(b.Name))
	}
	return nil
}

// ── config helper ─────────────────────────────────────────────────────────────

func loadConfig() (*config.Config, error) {
	cfgPath, err := ipc.ConfigFilePath()
	if err != nil {
		return nil, err
	}
	return config.Load(cfgPath)
}

func findBucketRegion(ctx context.Context, secretID, secretKey, bucket string) (string, error) {
	buckets, err := storage.ListBuckets(ctx, secretID, secretKey)
	if err != nil {
		return "", err
	}
	for _, b := range buckets {
		if b.Name == bucket {
			return b.Region, nil
		}
	}
	return "", fmt.Errorf("bucket %q not found in your account", bucket)
}
