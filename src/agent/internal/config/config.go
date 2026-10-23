package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config is the local agent configuration, stored as JSON in the agent dir.
type Config struct {
	// NodeID is generated on first run and persisted forever.
	NodeID string `json:"node_id"`

	// Server is the control plane address (scheme + host:port).
	Server string `json:"server"`

	// APIKey authenticates the agent against the control plane.
	APIKey string `json:"api_key"`

	// Zone is the placement zone (e.g. "home", "office").
	Zone string `json:"zone"`

	// CapacityBytes is the declared node capacity.
	CapacityBytes int64 `json:"capacity_bytes"`

	// Rack is the node rack/group. Empty means the server assigns one.
	Rack string `json:"rack"`

	// WeedBin is the path to the SeaweedFS weed binary.
	WeedBin string `json:"weed_bin"`

	// DataDir is where the volume server stores data on this node.
	DataDir string `json:"data_dir"`

	// Port is the volume server HTTP port managed by the agent.
	Port int `json:"port"`

	// MaxVolumes is the volume server -max (0 = SeaweedFS default, 8).
	MaxVolumes int `json:"max_volumes,omitempty"`

	// DiskType is the disk tag: hdd/ssd/arbitrary.
	DiskType string `json:"disk_type,omitempty"`

	// HeartbeatIntervalSec is the heartbeat interval.
	HeartbeatIntervalSec int `json:"heartbeat_interval_sec,omitempty"`

	// TLSInsecure skips certificate verification for HTTPS control plane
	// connections (development; production should use a trusted CA).
	TLSInsecure bool `json:"tls_insecure,omitempty"`
}

// Default returns a config with defaults (node_id still empty).
func Default() Config {
	return Config{
		APIKey:               "dev",
		Zone:                 "home",
		CapacityBytes:        200 * 1024 * 1024 * 1024, // 200 GB
		HeartbeatIntervalSec: 10,
		Port:                 8080,
	}
}

// Path returns the config file path inside baseDir.
func Path(baseDir string) string {
	return filepath.Join(baseDir, "agent.json")
}

// Load reads the config from baseDir/agent.json. If the file does not exist,
// it creates a config with a fresh node_id and persists it.
func Load(baseDir string) (*Config, error) {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("create agent dir %s: %w", baseDir, err)
	}
	path := Path(baseDir)
	cfg := Default()
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if cfg.DataDir == "" {
			cfg.DataDir = filepath.Join(baseDir, "data")
		}
		return &cfg, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	// First run: generate node_id and persist the config.
	id, err := newNodeID()
	if err != nil {
		return nil, err
	}
	cfg.NodeID = id
	cfg.DataDir = filepath.Join(baseDir, "data")
	if err := cfg.Save(baseDir); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Save writes the config to baseDir/agent.json (atomic replace).
func (c *Config) Save(baseDir string) error {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	path := Path(baseDir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func newNodeID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// node-<24 hex chars>
	return "node-" + hex.EncodeToString(b), nil
}
