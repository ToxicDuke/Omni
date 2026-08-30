package backup

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/apex/log"
	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/internal/models"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/server/filesystem"
)

type unavailableS3Client struct{}

func (unavailableS3Client) GetBackupRemoteUploadURLs(context.Context, string, int64) (remote.BackupRemoteUploadResponse, error) {
	return remote.BackupRemoteUploadResponse{}, errors.New("Garage is unavailable")
}
func (unavailableS3Client) GetInstallationScript(context.Context, string) (remote.InstallationScript, error) {
	return remote.InstallationScript{}, nil
}
func (unavailableS3Client) GetServerConfiguration(context.Context, string) (remote.ServerConfigurationResponse, error) {
	return remote.ServerConfigurationResponse{}, nil
}
func (unavailableS3Client) GetServers(context.Context, int) ([]remote.RawServerData, error) { return nil, nil }
func (unavailableS3Client) ResetServersState(context.Context) error                         { return nil }
func (unavailableS3Client) SetArchiveStatus(context.Context, string, bool) error           { return nil }
func (unavailableS3Client) SetBackupStatus(context.Context, string, remote.BackupRequest) error {
	return nil
}
func (unavailableS3Client) SendRestorationStatus(context.Context, string, bool) error { return nil }
func (unavailableS3Client) SetInstallationStatus(context.Context, string, remote.InstallStatusRequest) error {
	return nil
}
func (unavailableS3Client) SetTransferStatus(context.Context, string, bool) error { return nil }
func (unavailableS3Client) ValidateSftpCredentials(context.Context, remote.SftpAuthRequest) (remote.SftpAuthResponse, error) {
	return remote.SftpAuthResponse{}, nil
}
func (unavailableS3Client) SendActivityLogs(context.Context, []models.Activity) error { return nil }
func (unavailableS3Client) SetCredentials(string, string)                              {}

func TestS3GenerateRetainsArchiveAndFallsBackToLocalWhenUploadIsUnavailable(t *testing.T) {
	root := t.TempDir()
	backupDir := filepath.Join(root, "backups")
	serverDir := filepath.Join(root, "server")
	for _, dir := range []string{backupDir, serverDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(serverDir, "file.txt"), []byte("server data"), 0o600); err != nil {
		t.Fatal(err)
	}
	config.Set(&config.Configuration{System: config.SystemConfiguration{BackupDirectory: backupDir}})

	fsys, err := filesystem.New(serverDir, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := NewS3(unavailableS3Client{}, "11111111-1111-1111-1111-111111111111", "")

	details, err := b.Generate(context.Background(), fsys, "")
	if err != nil {
		t.Fatalf("expected local fallback, got %v", err)
	}
	if details.Adapter != LocalBackupAdapter {
		t.Fatalf("expected local fallback adapter %q, got %q", LocalBackupAdapter, details.Adapter)
	}
	if !details.RetainedLocally {
		t.Fatal("expected fallback archive to be marked for local retention")
	}
	if _, err := os.Stat(b.Path()); err != nil {
		t.Fatalf("expected fallback archive to be retained at %q: %v", b.Path(), err)
	}
	if request := details.ToRequest(true); request.Adapter != string(LocalBackupAdapter) {
		t.Fatalf("expected panel adapter %q, got %q", LocalBackupAdapter, request.Adapter)
	}
	if !details.ToRequest(true).FallbackToLocal {
		t.Fatal("expected panel request to identify the local fallback")
	}
}

func TestS3UploadPartRetriesFromTheSameArchiveOffset(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	if err := os.WriteFile(archive, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}

	var received [][]byte
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		received = append(received, body)
		if len(received) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("ETag", "part-etag")
		w.WriteHeader(http.StatusOK)
	}))
	defer endpoint.Close()

	uploader := newS3FileUploader(archive, log.WithField("test", t.Name()))
	etag, err := uploader.uploadPart(context.Background(), 1, endpoint.URL, 3, 4)
	if err != nil {
		t.Fatalf("expected retry to succeed: %v", err)
	}
	if etag != "part-etag" {
		t.Fatalf("expected ETag %q, got %q", "part-etag", etag)
	}
	if len(received) != 2 {
		t.Fatalf("expected two upload attempts, got %d", len(received))
	}
	for attempt, body := range received {
		if string(body) != "3456" {
			t.Fatalf("attempt %d sent %q; expected the complete section %q", attempt+1, body, "3456")
		}
	}
}
