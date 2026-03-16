package apiserver

import (
	"encoding/json"
	"net/http"
	"os"

	"github.com/cloudsync/cloudsync/internal/ipc"
	"go.uber.org/zap"
)

// MountManagerAPI is the interface the handlers require.
type MountManagerAPI interface {
	Add(localPath, remotePrefix string, downloadFirst bool, bucket string) (ipc.MountRecord, error)
	Remove(localPath string, deleteRemote bool) error
	List() []ipc.MountRecord
	Count() int
	DeleteObjects(remotePrefix string)
}

type handlers struct {
	mm     MountManagerAPI
	logger *zap.Logger
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// GET /status
func (h *handlers) status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"running":     true,
		"daemon_pid":  os.Getpid(),
		"mount_count": h.mm.Count(),
		"version":     "1.0.0",
	})
}

// GET /mounts → list; POST /mounts → add; DELETE /mounts → remove
func (h *handlers) mounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.listMounts(w, r)
	case http.MethodPost:
		h.addMount(w, r)
	case http.MethodDelete:
		h.removeMount(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (h *handlers) listMounts(w http.ResponseWriter, r *http.Request) {
	mounts := h.mm.List()
	if mounts == nil {
		mounts = []ipc.MountRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"mounts": mounts})
}

func (h *handlers) addMount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LocalPath     string `json:"local_path"`
		RemotePrefix  string `json:"remote_prefix"`
		DownloadFirst bool   `json:"download_first"`
		Bucket        string `json:"bucket"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.LocalPath == "" {
		writeError(w, http.StatusBadRequest, "local_path is required")
		return
	}

	rec, err := h.mm.Add(req.LocalPath, req.RemotePrefix, req.DownloadFirst, req.Bucket)
	if err != nil {
		h.logger.Warn("mount add failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (h *handlers) removeMount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LocalPath    string `json:"local_path"`
		DeleteRemote bool   `json:"delete_remote"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.LocalPath == "" {
		writeError(w, http.StatusBadRequest, "local_path is required")
		return
	}

	if err := h.mm.Remove(req.LocalPath, req.DeleteRemote); err != nil {
		h.logger.Warn("mount remove failed", zap.Error(err))
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// POST /objects/delete — fire-and-forget deletion of a COS prefix or key.
func (h *handlers) deleteObjects(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		RemotePrefix string `json:"remote_prefix"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.RemotePrefix == "" {
		writeError(w, http.StatusBadRequest, "remote_prefix is required")
		return
	}
	h.mm.DeleteObjects(req.RemotePrefix)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}
