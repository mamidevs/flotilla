package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyDefaults_FillsMissingFields(t *testing.T) {
	c := &Config{
		Nodes: []NodeConfig{{Name: "a", ExitNode: "a-exit"}},
	}
	c.ApplyDefaults()
	if c.Listen.Socks != DefaultSocksAddr {
		t.Errorf("Listen.Socks default: %q", c.Listen.Socks)
	}
	if c.Listen.HTTP != DefaultHTTPAddr {
		t.Errorf("Listen.HTTP default: %q", c.Listen.HTTP)
	}
	if c.Listen.Admin != DefaultAdminAddr {
		t.Errorf("Listen.Admin default: %q", c.Listen.Admin)
	}
	if c.Dispatch.Strategy != StrategyRoundRobin {
		t.Errorf("Dispatch.Strategy default: %q", c.Dispatch.Strategy)
	}
	if c.Auth.Method != AuthMethodAuthKey {
		t.Errorf("Auth.Method default (no oauth2 path): %q", c.Auth.Method)
	}
	if c.Nodes[0].StateDir == "" || !strings.Contains(c.Nodes[0].StateDir, "a") {
		t.Errorf("Node state_dir default: %q", c.Nodes[0].StateDir)
	}
}

func TestValidate_RejectsDuplicates(t *testing.T) {
	c := &Config{
		Nodes: []NodeConfig{
			{Name: "a", ExitNode: "x", StateDir: "/tmp/a"},
			{Name: "a", ExitNode: "y", StateDir: "/tmp/b"},
		},
	}
	c.ApplyDefaults()
	if err := c.Validate(); err == nil {
		t.Fatalf("expected duplicate-name error")
	}

	c2 := &Config{
		Nodes: []NodeConfig{
			{Name: "a", ExitNode: "x", StateDir: "/tmp/a"},
			{Name: "b", ExitNode: "y", StateDir: "/tmp/a"},
		},
	}
	c2.ApplyDefaults()
	if err := c2.Validate(); err == nil {
		t.Fatalf("expected duplicate-state_dir error")
	}
}

func TestValidate_RejectsBadStrategy(t *testing.T) {
	c := &Config{
		Dispatch: DispatchConfig{Strategy: "blueprint"},
		Nodes:    []NodeConfig{{Name: "a", ExitNode: "x", StateDir: "/tmp/a"}},
	}
	c.ApplyDefaults()
	c.Dispatch.Strategy = "blueprint" // overwrite after defaults
	if err := c.Validate(); err == nil {
		t.Fatalf("expected unknown-strategy error")
	}
}

func TestValidate_RequiresAtLeastOneNode(t *testing.T) {
	c := &Config{}
	c.ApplyDefaults()
	if err := c.Validate(); err == nil {
		t.Fatalf("expected error for empty Nodes")
	}
}

func TestLoad_ParsesYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flotilla.yaml")
	yaml := `
listen:
  socks: 127.0.0.1:6040
  http: 127.0.0.1:6050
  admin: 127.0.0.1:9080
dispatch:
  strategy: sticky
auth:
  method: authkey
nodes:
  - name: alpha
    exit_node: alpha-exit
    state_dir: ` + dir + `/alpha
    tags: [region:test]
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Dispatch.Strategy != StrategySticky {
		t.Errorf("strategy: %q", cfg.Dispatch.Strategy)
	}
	if len(cfg.Nodes) != 1 || cfg.Nodes[0].Name != "alpha" {
		t.Errorf("nodes: %+v", cfg.Nodes)
	}
}

func TestExampleYAML_IsValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flotilla.yaml")
	if err := os.WriteFile(path, ExampleYAML(), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("ExampleYAML is invalid: %v", err)
	}
	if len(cfg.Nodes) < 1 {
		t.Errorf("example yaml should have at least 1 node")
	}
}
