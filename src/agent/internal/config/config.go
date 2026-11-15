package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is the local agent configuration.
// Static settings (server, zone, capacity, paths, ports) come from
// /etc/s3lite/agent.env. Agent-managed state (node_id) lives in
// <baseDir>/agent.json. The two files must not overlap.
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

	// ECShardSlotRatio is the percent of volume slots reserved for erasure
	// coding shards (the rest hold raw volume replicas). 0 means no hint;
	// the server's DEFAULT_REPLICATION / MAX_VOLUMES still apply.
	ECShardSlotRatio int `json:"ec_shard_slot_ratio,omitempty"`

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
		ECShardSlotRatio:     30,
	}
}

// Path returns the config file path inside baseDir.
func Path(baseDir string) string {
	return filepath.Join(baseDir, "agent.json")
}

// envFilePath returns the path to the static environment file.
func envFilePath() string {
	if v := os.Getenv("AGENT_ENV_FILE"); v != "" {
		return v
	}
	return "/etc/s3lite/agent.env"
}

// parseEnvFile reads KEY=VALUE lines (comments start with #) and applies
// them to cfg. Static settings from the env file take precedence over
// agent.json; only agent-managed fields (node_id) live in agent.json.
func parseEnvFile(data []byte, cfg *Config) error {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		switch key {
		case "CONTROL_PLANE":
			cfg.Server = val
		case "API_KEY":
			cfg.APIKey = val
		case "ZONE":
			cfg.Zone = val
		case "CAPACITY_BYTES":
			if v, err := strconv.ParseInt(val, 10, 64); err == nil {
				cfg.CapacityBytes = v
			}
		case "RACK":
			cfg.Rack = val
		case "WEED_BIN":
			cfg.WeedBin = val
		case "STORAGE":
			cfg.DataDir = val
		case "STORAGE_SIZE":
			if v, err := strconv.ParseInt(val, 10, 64); err == nil {
				cfg.CapacityBytes = v * 1024 * 1024 * 1024
			}
		case "EC_SHARD_SLOT_RATIO":
			if v, err := strconv.Atoi(val); err == nil {
				cfg.ECShardSlotRatio = v
			}
		case "PORT":
			if v, err := strconv.Atoi(val); err == nil {
				cfg.Port = v
			}
		case "HEARTBEAT_INTERVAL_SEC":
			if v, err := strconv.Atoi(val); err == nil {
				cfg.HeartbeatIntervalSec = v
			}
		case "TLS_INSECURE":
			cfg.TLSInsecure = strings.EqualFold(val, "true")
		}
	}
	return nil
}

// Load reads configuration with the following priority (lowest to highest):
//   1. Default values (see Default)
//   2. /etc/s3lite/agent.env (static admin configuration)
//   3. <baseDir>/agent.json (agent-managed state, only node_id)
//
// Static fields from agent.env are never overridden by agent.json — the two
// files must not overlap.
func Load(baseDir string) (*Config, error) {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("create agent dir %s: %w", baseDir, err)
	}
	cfg := Default()

	// 1. Static configuration from /etc/s3lite/agent.env
	if data, err := os.ReadFile(envFilePath()); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read %s: %w (check file permissions — the s3lite user must be able to read it)", envFilePath(), err)
		}
	} else {
		if err := parseEnvFile(data, &cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", envFilePath(), err)
		}
	}

	// 2. Agent-managed state from agent.json (only node_id etc.)
	path := filepath.Join(baseDir, "agent.json")
	if data, err := os.ReadFile(path); err == nil {
		var agentState struct {
			NodeID string `json:"node_id"`
		}
		if err := json.Unmarshal(data, &agentState); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if agentState.NodeID != "" {
			cfg.NodeID = agentState.NodeID
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	// First run: generate node_id if not present anywhere.
	if cfg.NodeID == "" {
		id, err := newNodeID()
		if err != nil {
			return nil, fmt.Errorf("generate node id: %w", err)
		}
		cfg.NodeID = id
		if err := cfg.Save(baseDir); err != nil {
			return nil, fmt.Errorf("save config: %w", err)
		}
	}

	if cfg.DataDir == "" {
		cfg.DataDir = filepath.Join(baseDir, "data")
	}
	return &cfg, nil
}

// Save writes the agent-managed state (node_id) to baseDir/agent.json.
// Static settings from /etc/s3lite/agent.env are not persisted here.
func (c *Config) Save(baseDir string) error {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return err
	}
	// Only persist agent-managed fields, not static ones from .env.
	state := struct {
		NodeID string `json:"node_id"`
	}{
		NodeID: c.NodeID,
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(baseDir, "agent.json")
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
