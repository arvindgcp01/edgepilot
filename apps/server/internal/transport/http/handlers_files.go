package http

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/visionify/edgepilot/apps/server/internal/domain"
	"github.com/visionify/edgepilot/apps/server/internal/service/sftpfs"
)

// maxUploadBytes bounds one upload. Large enough for logs, packages, and
// configuration; not a substitute for a real deployment pipeline.
const maxUploadBytes = 512 << 20 // 512 MiB

// dialFiles opens an SFTP client to a device, trying each account the server's
// key is registered for — the same discovery the terminal uses, and for the
// same reason: the server cannot know which local account exists on a given Mac.
func (s *Server) dialFiles(r *http.Request, device *domain.Device) (*sftpfs.Client, string, error) {
	if s.terminalSigner == nil || s.tunnels == nil {
		return nil, "", domain.Unavailable("files_disabled",
			"file transfer is not enabled on this deployment")
	}
	usernames, err := s.terminalUsernames(r.Context())
	if err != nil {
		return nil, "", err
	}
	if len(usernames) == 0 {
		return nil, "", domain.Conflict("no_ssh_account",
			"no SSH account is registered for this server's key on the device")
	}

	var lastErr error
	for _, username := range usernames {
		tun, err := s.tunnels.Open(r.Context(), device.ID, "sftp:"+username, "127.0.0.1", 22, s.sshIdleTimeout)
		if err != nil {
			// A tunnel failure is about the device, not the account.
			return nil, "", err
		}
		client, err := sftpfs.Open(r.Context(), tun, sftpfs.Options{
			Signer: s.terminalSigner, Username: username, Log: s.log,
		})
		if err == nil {
			return client, username, nil
		}
		lastErr = err
		_ = tun.Close()
	}
	return nil, "", lastErr
}

type fileEntryResponse struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	IsDir   bool      `json:"is_dir"`
	ModTime time.Time `json:"mod_time"`
}

type listFilesResponse struct {
	Path     string              `json:"path"`
	Username string              `json:"username"`
	Entries  []fileEntryResponse `json:"entries"`
}

// listFiles returns a directory listing from the device. An empty path lists
// the account's home directory.
func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) error {
	device, err := s.approvedDevice(r)
	if err != nil {
		return err
	}
	client, username, err := s.dialFiles(r, device)
	if err != nil {
		return err
	}
	defer client.Close()

	dir := strings.TrimSpace(r.URL.Query().Get("path"))
	if dir == "" {
		if home, herr := client.Home(); herr == nil {
			dir = home
		} else {
			dir = "."
		}
	}

	entries, err := client.List(dir)
	if err != nil {
		return domain.NotFound("path", dir)
	}
	out := make([]fileEntryResponse, 0, len(entries))
	for _, e := range entries {
		// Hidden files are kept: an operator browsing a Mac to fetch a dotfile
		// or a log under a hidden directory is exactly the case this serves.
		out = append(out, fileEntryResponse(e))
	}
	return respond(w, http.StatusOK, listFilesResponse{Path: dir, Username: username, Entries: out})
}

// downloadFile streams one file from the device.
func (s *Server) downloadFile(w http.ResponseWriter, r *http.Request) error {
	device, err := s.approvedDevice(r)
	if err != nil {
		return err
	}
	filePath := strings.TrimSpace(r.URL.Query().Get("path"))
	if filePath == "" {
		return domain.Validation(domain.FieldError{Pointer: "/path", Message: "is required"})
	}

	client, _, err := s.dialFiles(r, device)
	if err != nil {
		return err
	}
	defer client.Close()

	reader, size, err := client.Open(filePath)
	if err != nil {
		return domain.NotFound("file", filePath)
	}
	defer reader.Close()

	name := path.Base(filePath)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	// A quoted filename, with quotes in the name stripped so the header cannot
	// be broken out of.
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s"`, strings.ReplaceAll(name, `"`, "")))
	w.WriteHeader(http.StatusOK)

	if _, err := io.Copy(w, reader); err != nil {
		// The header and status are already sent, so this only reaches the
		// access log — which is where a truncated download is worth seeing.
		s.log.WarnContext(r.Context(), "streaming a file download failed",
			slog.String("device_id", device.ID.String()), slog.Any("error", err))
		return err
	}
	return nil
}

// uploadFile writes a file to a directory on the device. The body is the file;
// the target directory and name are query parameters.
func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request) error {
	device, err := s.approvedDevice(r)
	if err != nil {
		return err
	}
	dir := strings.TrimSpace(r.URL.Query().Get("path"))
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if dir == "" || name == "" {
		return domain.Validation(domain.FieldError{Pointer: "/name", Message: "path and name are required"})
	}
	// The name is a single path element; a caller cannot walk out of the target
	// directory by smuggling separators into it.
	if strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return domain.Validation(domain.FieldError{Pointer: "/name", Message: "must be a plain file name"})
	}

	client, username, err := s.dialFiles(r, device)
	if err != nil {
		return err
	}
	defer client.Close()

	target := client.Join(dir, name)
	body := http.MaxBytesReader(w, r.Body, maxUploadBytes)
	written, err := client.Create(target, body)
	if err != nil {
		return domain.Conflict("upload_failed", "writing %s failed: %v", target, err)
	}

	s.log.InfoContext(r.Context(), "a file was uploaded to a device",
		slog.String("device_id", device.ID.String()),
		slog.String("target_username", username),
		slog.String("path", target),
		slog.Int64("bytes", written))

	return respond(w, http.StatusCreated, map[string]any{"path": target, "bytes": written})
}

// approvedDevice loads the device in the request path, scoped to the caller's
// organization, and refuses one that has not been approved.
func (s *Server) approvedDevice(r *http.Request) (*domain.Device, error) {
	id, err := pathUUID(r, "id")
	if err != nil {
		return nil, err
	}
	device, err := s.store.Devices().Get(r.Context(), id)
	if err != nil {
		return nil, err
	}
	if !device.Approved() {
		return nil, domain.Conflict("device_not_approved", "that device has not been approved")
	}
	return device, nil
}
