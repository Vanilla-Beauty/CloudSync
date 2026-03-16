package ipc

import (
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// MountRecord represents a single watched directory (shared between daemon and apiserver).
type MountRecord struct {
	ID           string    `json:"id"`
	LocalPath    string    `json:"local_path"`
	RemotePrefix string    `json:"remote_prefix"`
	Bucket       string    `json:"bucket,omitempty"` // empty means daemon default bucket
	AddedAt      time.Time `json:"added_at"`
}

// MountsFile is the structure persisted to mounts.json
type MountsFile struct {
	Mounts []MountRecord `json:"mounts"`
}

// ConfigDir returns the platform-appropriate config directory for cloudsync.
// Linux/macOS: ~/.config/cloudsync
// Windows:     %APPDATA%\cloudsync
func ConfigDir() (string, error) {
	var base string
	if runtime.GOOS == "windows" {
		base = os.Getenv("APPDATA")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			base = filepath.Join(home, "AppData", "Roaming")
		}
	} else {
		cfg := os.Getenv("XDG_CONFIG_HOME")
		if cfg != "" {
			base = cfg
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			base = filepath.Join(home, ".config")
		}
	}
	return filepath.Join(base, "cloudsync"), nil
}

// SocketPath returns the IPC path for the daemon.
// On Windows this is a Named Pipe path; on Unix it is a Unix Domain Socket.
func SocketPath() (string, error) {
	if runtime.GOOS == "windows" {
		return `\\.\pipe\cloudsyncd`, nil
	}
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cloudsyncd.sock"), nil
}

// ConfigFilePath returns the path to config.json.
func ConfigFilePath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// MountsFilePath returns the path to mounts.json.
func MountsFilePath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "mounts.json"), nil
}

// PIDFilePath returns the path to cloudsyncd.pid.
func PIDFilePath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cloudsyncd.pid"), nil
}

// LogFilePath returns the path to cloudsyncd.log.
func LogFilePath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cloudsyncd.log"), nil
}
