package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadExample(t *testing.T) {
	// The example config in configs/ must always parse successfully.
	// (Path is relative to the module root.)
	root, err := filepath.Abs("../../")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "configs", "higgsgo.example.toml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("example config missing (%v)", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Storage.Driver != "sqlite" {
		t.Errorf("default storage driver: got %q want sqlite", c.Storage.Driver)
	}
	if !c.Modes.Standalone {
		t.Errorf("modes.standalone should be true by default")
	}
	if c.Server.Listen == "" {
		t.Errorf("server.listen should not be empty")
	}
}

func TestValidateRejectsBadDriver(t *testing.T) {
	c := defaults()
	c.Storage.Driver = "mysql"
	if err := c.validate(); err == nil {
		t.Fatal("expected validate to reject driver=mysql")
	}
}

func TestValidateRequiresAMode(t *testing.T) {
	c := defaults()
	c.Modes.Standalone = false
	c.Modes.CPAPlugin = false
	if err := c.validate(); err == nil {
		t.Fatal("expected validate to reject when both modes are disabled")
	}
}
