package daemon

// StatusResponse is returned by GET /status
type StatusResponse struct {
	Running    bool   `json:"running"`
	DaemonPID  int    `json:"daemon_pid"`
	MountCount int    `json:"mount_count"`
	Version    string `json:"version"`
}

// MountRequest is the body for POST /mounts
type MountRequest struct {
	LocalPath    string `json:"local_path"`
	RemotePrefix string `json:"remote_prefix"`
}

// UnmountRequest is the body for DELETE /mounts
type UnmountRequest struct {
	LocalPath    string `json:"local_path"`
	DeleteRemote bool   `json:"delete_remote"`
}

// ErrorResponse is returned on error
type ErrorResponse struct {
	Error string `json:"error"`
}
