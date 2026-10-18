package docker

import "testing"

func TestNewBindIPNetwork(t *testing.T) {
	n, err := newBindIPNetwork("193.239.179.113")
	if err != nil {
		t.Fatal(err)
	}

	if n.Address != "193.239.179.113" {
		t.Fatalf("unexpected address: %s", n.Address)
	}
	if n.Subnet != "193.239.179.112/30" {
		t.Fatalf("unexpected subnet: %s", n.Subnet)
	}
	if n.Gateway != "193.239.179.114" {
		t.Fatalf("unexpected gateway: %s", n.Gateway)
	}
	if n.Name != "pterodactyl_bind_193_239_179_113" {
		t.Fatalf("unexpected name: %s", n.Name)
	}
}

func TestNewBindIPNetworkRejectsInvalidAddresses(t *testing.T) {
	for _, value := range []string{"", "not-an-ip", "127.0.0.1", "193.239.179.112", "193.239.179.115"} {
		if _, err := newBindIPNetwork(value); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}
