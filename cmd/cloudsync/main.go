package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudsync/cloudsync/internal/apiclient"
	"github.com/cloudsync/cloudsync/internal/config"
	"github.com/cloudsync/cloudsync/internal/daemon"
	"github.com/cloudsync/cloudsync/internal/ipc"
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
		unmountCmd(),
		deleteCmd(),
		lsCmd(),
	)
	return root
}

// ── init ──────────────────────────────────────────────────────────────────────

func initCmd() *cobra.Command {
	var (
		secretID  string
		secretKey string
		bucket    string
		region    string
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Configure COS credentials and write config.json",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit(secretID, secretKey, bucket, region)
		},
	}
	cmd.Flags().StringVar(&secretID, "secret-id", "", "COS SecretId")
	cmd.Flags().StringVar(&secretKey, "secret-key", "", "COS SecretKey")
	cmd.Flags().StringVar(&bucket, "bucket", "", "COS bucket name")
	cmd.Flags().StringVar(&region, "region", "", "COS region (default: ap-guangzhou)")
	return cmd
}

func runInit(secretID, secretKey, bucket, region string) error {
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
	bucket = prompt("COS Bucket", bucket)
	if region == "" {
		fmt.Print("COS Region [ap-guangzhou]: ")
		v, _ := reader.ReadString('\n')
		v = strings.TrimSpace(v)
		if v == "" {
			v = "ap-guangzhou"
		}
		region = v
	}

	cfg := config.DefaultConfig()
	cfg.COS.SecretID = secretID
	cfg.COS.SecretKey = secretKey
	cfg.COS.Bucket = bucket
	cfg.COS.Region = region

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
	var prefix string
	cmd := &cobra.Command{
		Use:   "mount <path>",
		Short: "Start syncing a local directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMount(args[0], prefix)
		},
	}
	cmd.Flags().StringVar(&prefix, "prefix", "", "Remote prefix (default: basename of path + /)")
	return cmd
}

func runMount(path, prefix string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	if prefix == "" {
		prefix = filepath.Base(absPath) + "/"
	}

	client, err := newClient()
	if err != nil {
		return err
	}
	rec, err := client.AddMount(absPath, prefix)
	if err != nil {
		return err
	}
	fmt.Printf("Mounted: %s → %s (id: %s)\n", rec.LocalPath, rec.RemotePrefix, rec.ID)
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
	fmt.Printf("%-10s  %-40s  %-20s  %s\n", "ID", "LOCAL PATH", "REMOTE PREFIX", "ADDED AT")
	fmt.Println(strings.Repeat("-", 90))
	for _, m := range mounts {
		fmt.Printf("%-10s  %-40s  %-20s  %s\n",
			m.ID, m.LocalPath, m.RemotePrefix, m.AddedAt.Local().Format(time.RFC3339))
	}
}
