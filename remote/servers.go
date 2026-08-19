package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/pterodactyl/wings/internal/models"

	"emperror.dev/errors"
	"github.com/apex/log"
	"golang.org/x/sync/errgroup"
)

const (
	ProcessStopCommand    = "command"
	ProcessStopSignal     = "signal"
	ProcessStopNativeStop = "stop"
)

// GetServers returns all the servers that are present on the Panel making
// parallel API calls to the endpoint if more than one page of servers is
// returned.
func (c *client) GetServers(ctx context.Context, limit int) ([]RawServerData, error) {
	servers, meta, err := c.getServersPaged(ctx, 0, limit)
	if err != nil {
		return c.cachedServers(ctx, err)
	}

	var mu sync.Mutex
	if meta.LastPage > 1 {
		g, ctx := errgroup.WithContext(ctx)
		for page := meta.CurrentPage + 1; page <= meta.LastPage; page++ {
			page := page
			g.Go(func() error {
				ps, _, err := c.getServersPaged(ctx, int(page), limit)
				if err != nil {
					return err
				}
				mu.Lock()
				servers = append(servers, ps...)
				mu.Unlock()
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return c.cachedServers(ctx, err)
		}
	}
	c.cacheServers(ctx, servers)

	return servers, nil
}

// ResetServersState updates the state of all servers on the node that are
// currently marked as "installing" or "restoring from backup" to be marked as
// a normal successful install state.
//
// This handles Wings exiting during either of these processes which will leave
// things in a bad state within the Panel. This API call is executed once Wings
// has fully booted all the servers.
func (c *client) ResetServersState(ctx context.Context) error {
	if err := c.postEvent(ctx, "reset-server-states", "/servers/reset", nil); err != nil {
		return errors.WrapIf(err, "remote: failed to reset server state on Panel")
	}
	return nil
}

func (c *client) GetServerConfiguration(ctx context.Context, uuid string) (ServerConfigurationResponse, error) {
	var configuration ServerConfigurationResponse
	res, err := c.Get(ctx, fmt.Sprintf("/servers/%s", uuid), nil)
	if err != nil {
		if c.store != nil && isTransientPanelError(err) {
			if payload, cachedAt, cacheErr := c.store.LoadServerConfiguration(ctx, uuid); cacheErr == nil {
				if jsonErr := json.Unmarshal(payload, &configuration); jsonErr == nil {
					log.WithFields(log.Fields{"server": uuid, "cached_at": cachedAt}).Warn("using cached server configuration")
					return configuration, nil
				}
			}
		}
		return configuration, err
	}
	defer res.Body.Close()

	err = res.BindJSON(&configuration)
	if err == nil && c.store != nil {
		if payload, marshalErr := json.Marshal(configuration); marshalErr == nil {
			if cacheErr := c.store.SaveServerConfiguration(ctx, uuid, payload); cacheErr != nil {
				log.WithError(cacheErr).WithField("server", uuid).Error("failed to cache server configuration")
			}
		}
	}
	return configuration, err
}

func (c *client) GetInstallationScript(ctx context.Context, uuid string) (InstallationScript, error) {
	res, err := c.Get(ctx, fmt.Sprintf("/servers/%s/install", uuid), nil)
	if err != nil {
		return InstallationScript{}, err
	}
	defer res.Body.Close()

	var config InstallationScript
	err = res.BindJSON(&config)
	return config, err
}

func (c *client) SetInstallationStatus(ctx context.Context, uuid string, data InstallStatusRequest) error {
	return c.postEvent(ctx, "installation-status", fmt.Sprintf("/servers/%s/install", uuid), data)
}

func (c *client) SetArchiveStatus(ctx context.Context, uuid string, successful bool) error {
	return c.postEvent(ctx, "archive-status", fmt.Sprintf("/servers/%s/archive", uuid), d{"successful": successful})
}

func (c *client) SetTransferStatus(ctx context.Context, uuid string, successful bool) error {
	state := "failure"
	if successful {
		state = "success"
	}
	return c.postEvent(ctx, "transfer-status", fmt.Sprintf("/servers/%s/transfer/%s", uuid, state), nil)
}

// ValidateSftpCredentials makes a request to determine if the username and
// password combination provided is associated with a valid server on the instance
// using the Panel's authentication control mechanisms. This will get itself
// throttled if too many requests are made, allowing us to completely offload
// all the authorization security logic to the Panel.
func (c *client) ValidateSftpCredentials(ctx context.Context, request SftpAuthRequest) (SftpAuthResponse, error) {
	var auth SftpAuthResponse
	res, err := c.Post(ctx, "/sftp/auth", request)
	if err != nil {
		if err := AsRequestError(err); err != nil && (err.StatusCode() >= 400 && err.StatusCode() < 500) {
			log.WithFields(log.Fields{"subsystem": "sftp", "username": request.User, "ip": request.IP}).Warn(err.Error())
			return auth, &SftpInvalidCredentialsError{}
		}
		return auth, err
	}
	defer res.Body.Close()

	if err := res.BindJSON(&auth); err != nil {
		return auth, err
	}
	return auth, nil
}

func (c *client) GetBackupRemoteUploadURLs(ctx context.Context, backup string, size int64) (BackupRemoteUploadResponse, error) {
	var data BackupRemoteUploadResponse
	res, err := c.Get(ctx, fmt.Sprintf("/backups/%s", backup), q{"size": strconv.FormatInt(size, 10)})
	if err != nil {
		return data, err
	}
	defer res.Body.Close()
	if err := res.BindJSON(&data); err != nil {
		return data, err
	}
	return data, nil
}

func (c *client) SetBackupStatus(ctx context.Context, backup string, data BackupRequest) error {
	return c.postEvent(ctx, "backup-status", fmt.Sprintf("/backups/%s", backup), data)
}

// SendRestorationStatus triggers a request to the Panel to notify it that a
// restoration has been completed and the server should be marked as being
// activated again.
func (c *client) SendRestorationStatus(ctx context.Context, backup string, successful bool) error {
	return c.postEvent(ctx, "restoration-status", fmt.Sprintf("/backups/%s/restore", backup), d{"successful": successful})
}

func (c *client) postEvent(ctx context.Context, kind, path string, data interface{}) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	response, err := c.Post(ctx, path, data)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err == nil || c.store == nil || !isTransientPanelError(err) {
		return err
	}
	queueCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if queueErr := c.store.Enqueue(queueCtx, kind, "POST", path, payload, err.Error()); queueErr != nil {
		return errors.Wrap(queueErr, "remote: failed to queue undelivered Panel event")
	}
	log.WithFields(log.Fields{"kind": kind, "path": path}).Warn("queued undelivered Panel event")
	return nil
}

func isTransientPanelError(err error) bool {
	requestError := AsRequestError(err)
	return requestError == nil || requestError.StatusCode() >= 500
}

func (c *client) cacheServers(ctx context.Context, servers []RawServerData) {
	if c.store == nil {
		return
	}
	uuids := make([]string, 0, len(servers))
	for _, server := range servers {
		uuids = append(uuids, server.Uuid)
		configuration := ServerConfigurationResponse{Settings: server.Settings}
		if err := json.Unmarshal(server.ProcessConfiguration, &configuration.ProcessConfiguration); err != nil {
			continue
		}
		payload, err := json.Marshal(configuration)
		if err == nil {
			if err := c.store.SaveServerConfiguration(ctx, server.Uuid, payload); err != nil {
				log.WithError(err).WithField("server", server.Uuid).Error("failed to cache server configuration from Panel snapshot")
			}
		}
	}
	if err := c.store.PruneServerConfigurations(ctx, uuids); err != nil {
		log.WithError(err).Error("failed to prune stale server configurations")
	}
}

func (c *client) cachedServers(ctx context.Context, cause error) ([]RawServerData, error) {
	if c.store == nil || !isTransientPanelError(cause) {
		return nil, cause
	}
	entries, err := c.store.LoadAllServerConfigurations(ctx)
	if err != nil || len(entries) == 0 {
		return nil, cause
	}
	servers := make([]RawServerData, 0, len(entries))
	for _, entry := range entries {
		var configuration ServerConfigurationResponse
		if err := json.Unmarshal(entry.Payload, &configuration); err != nil || configuration.ProcessConfiguration == nil {
			continue
		}
		process, err := json.Marshal(configuration.ProcessConfiguration)
		if err != nil {
			continue
		}
		servers = append(servers, RawServerData{Uuid: entry.UUID, Settings: configuration.Settings, ProcessConfiguration: process})
	}
	if len(servers) == 0 {
		return nil, cause
	}
	log.WithField("total_configs", len(servers)).Warn("booting with cached server configurations")
	return servers, nil
}

// SendActivityLogs sends activity logs back to the Panel for processing.
func (c *client) SendActivityLogs(ctx context.Context, activity []models.Activity) error {
	resp, err := c.Post(ctx, "/activity", d{"data": activity})
	if err != nil {
		return errors.WithStackIf(err)
	}
	_ = resp.Body.Close()
	return nil
}

// getServersPaged returns a subset of servers from the Panel API using the
// pagination query parameters.
func (c *client) getServersPaged(ctx context.Context, page, limit int) ([]RawServerData, Pagination, error) {
	var r struct {
		Data []RawServerData `json:"data"`
		Meta Pagination      `json:"meta"`
	}

	res, err := c.Get(ctx, "/servers", q{
		"page":     strconv.Itoa(page),
		"per_page": strconv.Itoa(limit),
	})
	if err != nil {
		return nil, r.Meta, err
	}
	defer res.Body.Close()
	if err := res.BindJSON(&r); err != nil {
		return nil, r.Meta, err
	}
	return r.Data, r.Meta, nil
}
