package config

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// PanelEndpoints returns the configured Panel endpoints in deterministic
// priority order. The legacy remote value is used when no endpoint list exists.
func (c *Configuration) PanelEndpoints() ([]PanelEndpointConfiguration, error) {
	endpoints := append([]PanelEndpointConfiguration(nil), c.PanelFailover.Endpoints...)
	if len(endpoints) == 0 && strings.TrimSpace(c.PanelLocation) != "" {
		endpoints = []PanelEndpointConfiguration{{
			Name: "legacy",
			URL:  c.PanelLocation,
		}}
	}
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("config: at least one Panel endpoint is required")
	}

	names := make(map[string]struct{}, len(endpoints))
	for i := range endpoints {
		endpoints[i].Name = strings.TrimSpace(endpoints[i].Name)
		endpoints[i].URL = strings.TrimRight(strings.TrimSpace(endpoints[i].URL), "/")
		if endpoints[i].Name == "" {
			return nil, fmt.Errorf("config: Panel endpoint %d has no name", i)
		}
		if _, ok := names[endpoints[i].Name]; ok {
			return nil, fmt.Errorf("config: duplicate Panel endpoint name %q", endpoints[i].Name)
		}
		names[endpoints[i].Name] = struct{}{}

		u, err := url.ParseRequestURI(endpoints[i].URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("config: Panel endpoint %q has an invalid URL", endpoints[i].Name)
		}
	}

	sort.SliceStable(endpoints, func(i, j int) bool {
		return endpoints[i].Priority < endpoints[j].Priority
	})
	return endpoints, nil
}
