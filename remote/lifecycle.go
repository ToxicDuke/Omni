package remote

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"github.com/apex/log"
)

// Start restores the selected endpoint and runs health and outbox workers until
// the application context is cancelled.
func (c *client) Start(ctx context.Context) {
	c.startOnce.Do(func() {
		c.restoreActiveEndpoint(ctx)
		go c.runHealthChecks(ctx)
		if c.store != nil {
			go c.runOutbox(ctx)
		}
	})
}

func (c *client) restoreActiveEndpoint(ctx context.Context) {
	if c.store == nil {
		return
	}
	name, err := c.store.LoadActiveEndpoint(ctx)
	if err != nil {
		log.WithError(err).Error("failed to restore active Panel endpoint")
		return
	}
	if name == "" {
		c.mu.RLock()
		initial := c.endpoints[c.active].Name
		c.mu.RUnlock()
		if err := c.store.SaveEndpointSwitch(ctx, "", initial, "initial-selection"); err != nil {
			log.WithError(err).Error("failed to persist initial Panel endpoint")
		}
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.endpoints {
		if c.endpoints[i].Name == name {
			c.active = i
			c.baseUrl = c.endpoints[i].URL
			log.WithField("endpoint", name).Info("restored active Panel endpoint")
			return
		}
	}
	log.WithField("endpoint", name).Warn("persisted Panel endpoint is no longer configured")
	initial := c.endpoints[0].Name
	if err := c.store.SaveEndpointSwitch(ctx, name, initial, "configured-endpoint-removed"); err != nil {
		log.WithError(err).Error("failed to replace removed persisted Panel endpoint")
	}
}

func (c *client) runHealthChecks(ctx context.Context) {
	ticker := time.NewTicker(c.healthInterval)
	defer ticker.Stop()
	c.healthCheckAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.healthCheckAll(ctx)
		}
	}
}

func (c *client) healthCheckAll(ctx context.Context) {
	c.mu.RLock()
	endpoints := append([]Endpoint(nil), c.endpoints...)
	c.mu.RUnlock()
	for i, endpoint := range endpoints {
		checkCtx, cancel := context.WithTimeout(ctx, min(c.healthInterval, 10*time.Second))
		response, err := c.requestOnceAgainst(checkCtx, endpoint.URL, http.MethodGet, "/servers", nil, func(request *http.Request) {
			query := request.URL.Query()
			query.Set("page", "1")
			query.Set("per_page", "1")
			request.URL.RawQuery = query.Encode()
		})
		cancel()
		healthy := err == nil && response != nil && (response.StatusCode < 300 || response.StatusCode == http.StatusTooManyRequests)
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		c.recordHealthResult(i, healthy)
	}
}

func (c *client) recordHealthResult(index int, healthy bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if index < 0 || index >= len(c.endpoints) {
		return
	}
	if healthy {
		c.failures[index] = 0
		c.successes[index]++
		if c.successes[index] >= c.recoveryThreshold {
			c.unhealthy[index] = false
			if index < c.active && time.Since(c.lastSwitch) >= c.switchCooldown {
				c.switchLocked(index, "preferred-endpoint-recovered")
			}
		}
		return
	}

	c.successes[index] = 0
	c.failures[index]++
	if c.failures[index] >= c.failureThreshold {
		c.unhealthy[index] = true
		if index == c.active {
			c.switchToNextLocked("health-check-failed")
		}
	}
}

func (c *client) runOutbox(ctx context.Context) {
	interval := c.healthInterval
	if interval > 10*time.Second {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	c.replayOutbox(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.replayOutbox(ctx)
		}
	}
}

func (c *client) replayOutbox(ctx context.Context) {
	events, err := c.store.DueOutbox(ctx, 100)
	if err != nil {
		log.WithError(err).Error("failed to load Panel event outbox")
		return
	}
	for _, event := range events {
		response, err := c.request(ctx, event.Method, event.Path, bytes.NewBuffer(event.Payload))
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if err == nil {
			if err := c.store.MarkOutboxDelivered(ctx, event.ID); err != nil {
				log.WithError(err).WithField("event_id", event.ID).Error("failed to remove delivered Panel event")
			}
			continue
		}
		delay := time.Second * time.Duration(1<<min(event.Attempts, 8))
		if err := c.store.MarkOutboxFailed(ctx, event.ID, err.Error(), time.Now().Add(delay)); err != nil {
			log.WithError(err).WithField("event_id", event.ID).Error("failed to update Panel event retry")
		}
		// Preserve ordering: later state events must not overtake an older event.
		break
	}
}

func (c *client) Metrics(ctx context.Context) Metrics {
	c.mu.RLock()
	metrics := Metrics{ActiveEndpoint: c.endpoints[c.active].Name, ActivePriority: c.endpoints[c.active].Priority, Switches: c.switches}
	metrics.Endpoints = make([]EndpointMetrics, len(c.endpoints))
	for i, endpoint := range c.endpoints {
		metrics.Endpoints[i] = EndpointMetrics{
			Name: endpoint.Name, Priority: endpoint.Priority, Healthy: !c.unhealthy[i],
			ConsecutiveFailures: c.failures[i], ConsecutiveSuccesses: c.successes[i],
		}
	}
	c.mu.RUnlock()
	if c.store == nil {
		return metrics
	}
	if count, oldest, err := c.store.OutboxStats(ctx); err == nil {
		metrics.OutboxDepth = count
		if !oldest.IsZero() {
			metrics.OldestOutboxAt = &oldest
			metrics.OldestOutboxAgeSeconds = max(0, int64(time.Since(oldest).Seconds()))
		}
	}
	if count, oldest, err := c.store.CacheStats(ctx); err == nil {
		metrics.CachedServers = count
		if !oldest.IsZero() {
			metrics.OldestCacheAt = &oldest
			metrics.OldestCacheAgeSeconds = max(0, int64(time.Since(oldest).Seconds()))
		}
	}
	if switches, err := c.store.RecentEndpointSwitches(ctx, 20); err == nil {
		metrics.RecentSwitches = make([]SwitchRecord, len(switches))
		for i, item := range switches {
			metrics.RecentSwitches[i] = SwitchRecord{From: item.From, To: item.To, Reason: item.Reason, CreatedAt: item.CreatedAt}
		}
	}
	if count, err := c.store.EndpointSwitchCount(ctx); err == nil {
		metrics.Switches = uint64(count)
	}
	return metrics
}
