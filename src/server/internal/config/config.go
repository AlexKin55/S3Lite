package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the static control-plane configuration, loaded from
// /etc/s3lite/server.env. Agent/operator must not override these via CLI.
// agent.json/node state is managed separately by the server registry.
type Config struct {
	Listen          string
	TLSCert         string
	TLSKey          string
	Master          string
	Filer           string
	PollSec         int
	HeartbeatTTL    int
	MinWriteNodes   int
	WebhookURL      string
	DataCenter      string
	RackBase        string
	MaxVolumes      int
	DiskType        string
	PortBase        int
	StateFile       string
	PreferredNodesFile string
	PreferredPolicyFile string
	AdminURL        string
	WeedBin         string
	FsckTimeout     time.Duration
}

const defaultEnvFile = "/etc/s3lite/server.env"

// Default returns a config with compile-time defaults.
func Default() Config {
	return Config{
		Listen:            ":8099",
		Master:            "192.168.1.9:9333",
		Filer:             "127.0.0.1:8888",
		PollSec:           15,
		HeartbeatTTL:      60,
		MinWriteNodes:     10,
		DataCenter:        "dc1",
		RackBase:          "rack",
		MaxVolumes:        8,
		DiskType:          "hdd",
		PortBase:          8080,
		WeedBin:           "/usr/bin/weed",
		FsckTimeout:       10 * time.Minute,
		AdminURL:          "http://127.0.0.1:12535",
	}
}

// envFilePath returns the path to the static environment file.
func envFilePath() string {
	if v := os.Getenv("SERVER_ENV_FILE"); v != "" {
		return v
	}
	return defaultEnvFile
}

// parseEnvFile reads KEY=VALUE lines (comments start with #) and applies
// them to cfg. Static settings from the env file always win — CLI flags
// cannot override them (they are removed entirely).
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
		case "LISTEN":
			cfg.Listen = val
		case "TLS_CERT":
			cfg.TLSCert = val
		case "TLS_KEY":
			cfg.TLSKey = val
		case "MASTER":
			cfg.Master = val
		case "FILER":
			cfg.Filer = val
		case "POLL_SEC":
			if v, err := strconv.Atoi(val); err == nil {
				cfg.PollSec = v
			}
		case "HEARTBEAT_TTL":
			if v, err := strconv.Atoi(val); err == nil {
				cfg.HeartbeatTTL = v
			}
		case "MIN_WRITE_NODES":
			if v, err := strconv.Atoi(val); err == nil {
				cfg.MinWriteNodes = v
			}
		case "WEBHOOK_URL":
			cfg.WebhookURL = val
		case "DATA_CENTER":
			cfg.DataCenter = val
		case "RACK_BASE":
			cfg.RackBase = val
		case "MAX_VOLUMES":
			if v, err := strconv.Atoi(val); err == nil {
				cfg.MaxVolumes = v
			}
		case "DISK_TYPE":
			cfg.DiskType = val
		case "PORT_BASE":
			if v, err := strconv.Atoi(val); err == nil {
				cfg.PortBase = v
			}
		case "STATE_FILE":
			cfg.StateFile = val
		case "PREFERRED_NODES_FILE":
			cfg.PreferredNodesFile = val
		case "PREFERRED_POLICY_FILE":
			cfg.PreferredPolicyFile = val
		case "ADMIN_URL":
			cfg.AdminURL = val
		case "WEED_BIN":
			cfg.WeedBin = val
		case "FSCK_TIMEOUT":
			if v, err := strconv.ParseInt(val, 10, 64); err == nil {
				cfg.FsckTimeout = time.Duration(v) * time.Second
			}
		}
	}
	return nil
}

// Load reads configuration with the following priority (lowest to highest):
//   1. Default values (see Default)
//   2. /etc/s3lite/server.env (static admin configuration)
//
// CLI flags are not accepted for static settings — the two sources must
// not overlap.
func Load() (*Config, error) {
	cfg := Default()

	if data, err := os.ReadFile(envFilePath()); err != nil {
		if os.IsNotExist(err) {
			// File not installed yet — fall back to compile-time defaults.
			// This is acceptable before the package conffile is in place.
		} else {
			return nil, fmt.Errorf("read %s: %w (check file permissions — the s3lite user must be able to read it)", envFilePath(), err)
		}
	} else {
		if err := parseEnvFile(data, &cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", envFilePath(), err)
		}
	}

	return &cfg, nil
}
