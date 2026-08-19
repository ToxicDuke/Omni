package config

import "testing"

func TestPanelEndpointsLegacyCompatibility(t *testing.T) {
	c := Configuration{PanelLocation: "https://panel.example.com/"}
	endpoints, err := c.PanelEndpoints()
	if err != nil {
		t.Fatal(err)
	}
	if len(endpoints) != 1 || endpoints[0].Name != "legacy" || endpoints[0].URL != "https://panel.example.com" {
		t.Fatalf("unexpected endpoints: %#v", endpoints)
	}
}

func TestPanelEndpointsPriorityOrder(t *testing.T) {
	c := Configuration{PanelFailover: PanelFailoverConfiguration{Endpoints: []PanelEndpointConfiguration{
		{Name: "pl", URL: "https://pl.example.com", Priority: 20},
		{Name: "ru", URL: "https://ru.example.com", Priority: 10},
		{Name: "ru-secondary", URL: "https://ru-2.example.com", Priority: 10},
	}}}
	endpoints, err := c.PanelEndpoints()
	if err != nil {
		t.Fatal(err)
	}
	if endpoints[0].Name != "ru" || endpoints[1].Name != "ru-secondary" || endpoints[2].Name != "pl" {
		t.Fatalf("unexpected endpoint order: %#v", endpoints)
	}
}

func TestPanelEndpointsRejectInvalidConfiguration(t *testing.T) {
	tests := []Configuration{
		{},
		{PanelFailover: PanelFailoverConfiguration{Endpoints: []PanelEndpointConfiguration{{Name: "ru", URL: "not-a-url"}}}},
		{PanelFailover: PanelFailoverConfiguration{Endpoints: []PanelEndpointConfiguration{
			{Name: "same", URL: "https://one.example.com"},
			{Name: "same", URL: "https://two.example.com"},
		}}},
	}
	for _, c := range tests {
		if _, err := c.PanelEndpoints(); err == nil {
			t.Fatalf("expected invalid configuration to fail: %#v", c)
		}
	}
}
