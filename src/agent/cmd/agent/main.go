package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"s3lite.dev/agent/internal/api"
	"s3lite.dev/agent/internal/config"
	"s3lite.dev/agent/internal/supervisor"
)

func main() {
	agentDir := flag.String("dir", ".cluster/agent", "agent directory: config, data, logs")
	serverAddr := flag.String("server", "", "control plane address (overrides config)")
	port := flag.Int("port", 0, "volume server port (overrides config)")
	weedBin := flag.String("weed", "", "path to the weed binary (overrides config)")
	storageDir := flag.String("storage", "", "volume data directory (overrides config)")
	capacityMB := flag.Int64("capacity", 0, "node capacity in megabytes (overrides config)")
	insecureTLS := flag.Bool("insecure", false, "skip TLS certificate verification for HTTPS control plane")
	flag.Parse()

	if err := run(*agentDir, *serverAddr, *port, *weedBin, *storageDir, *capacityMB, *insecureTLS); err != nil {
		fmt.Fprintf(os.Stderr, "agent: %v\n", err)
		os.Exit(1)
	}
}

func run(agentDir, serverOverride string, portOverride int, weedOverride, storageOverride string, capacityOverrideMB int64, insecureTLS bool) error {
	agentDir = abs(agentDir)

	cfg, err := config.Load(agentDir)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if serverOverride != "" {
		cfg.Server = serverOverride
	}
	if portOverride > 0 {
		cfg.Port = portOverride
	}
	if weedOverride != "" {
		cfg.WeedBin = weedOverride
	}
	if cfg.WeedBin == "" {
		return fmt.Errorf("weed binary path is empty: pass -weed or set weed_bin in %s", config.Path(agentDir))
	}
	if storageOverride != "" {
		cfg.DataDir = storageOverride
	}
	if capacityOverrideMB > 0 {
		cfg.CapacityBytes = capacityOverrideMB * 1024 * 1024
	}

	if insecureTLS {
		cfg.TLSInsecure = true
	}
	client := api.New(cfg.Server, cfg.APIKey, cfg.TLSInsecure)

	// The local IP is also the address the volume server advertises in the cluster.
	ip, err := supervisor.DetectIP()
	if err != nil {
		return err
	}

	dataDir := cfg.DataDir
	if dataDir == "" {
		dataDir = filepath.Join(agentDir, "data")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	logPath := filepath.Join(agentDir, "volume.log")

	fmt.Printf("agent %s: server=%s ip=%s port=%d zone=%s cap=%dMiB\n",
		cfg.NodeID, cfg.Server, ip, cfg.Port, cfg.Zone, cfg.CapacityBytes/(1024*1024))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Register the node (re-register if the IP changed).
	volCfg, err := register(ctx, client, cfg, ip)
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}

	volumePort := pickPort(volCfg.VolumePort, cfg.Port)
	sup := supervisor.New(supervisor.VolumeArgs{
		WeedBin:    cfg.WeedBin,
		IP:         ip,
		Port:       volumePort,
		Master:     volCfg.Master,
		Dir:        dataDir,
		DataCenter: volCfg.DataCenter,
		Rack:       pickRack(cfg.Rack, volCfg.Rack),
		MaxVolumes: pickMax(cfg.MaxVolumes, volCfg.MaxVolumes),
		DiskType:   pickDisk(cfg.DiskType, volCfg.DiskType),
		LogPath:    logPath,
	})

	if err := sup.Start(2 * time.Second); err != nil {
		return err
	}
	fmt.Printf("volume server started on %s:%d (master %s)\n", ip, volumePort, volCfg.Master)

	interval := time.Duration(cfg.HeartbeatIntervalSec) * time.Second
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	lastIP := ip
	for {
		select {
		case <-ctx.Done():
			fmt.Println("agent: stopping")
			return sup.Stop()
		case <-ticker.C:
			// IP changed => re-register and restart the volume server.
			newIP, err := supervisor.DetectIP()
			if err == nil && newIP != lastIP {
				fmt.Printf("agent: IP changed %s -> %s, re-registering\n", lastIP, newIP)
				volCfg, err = register(ctx, client, cfg, newIP)
				if err != nil {
					fmt.Printf("agent: re-register failed: %v\n", err)
					continue
				}
				lastIP = newIP
				volArgs := sup.Args()
				volArgs.IP = newIP
				if err := sup.Restart(volArgs, 2*time.Second); err != nil {
					fmt.Printf("agent: restart after IP change failed: %v\n", err)
				}
				continue
			}

			// Heartbeat with live statistics.
			hb := heartbeatPayload(sup, dataDir, cfg.CapacityBytes)
			if err := client.Heartbeat(ctx, cfg.NodeID, hb); err != nil {
				fmt.Printf("agent: heartbeat error: %v\n", err)
			}
		}
	}
}

// register performs the (re-)registration and returns the volume config.
func register(ctx context.Context, client *api.Client, cfg *config.Config, ip string) (*api.VolumeConfig, error) {
	resp, err := client.Register(ctx, api.RegisterRequest{
		NodeID:        cfg.NodeID,
		CapacityBytes: cfg.CapacityBytes,
		Zone:          cfg.Zone,
		IP:            ip,
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
	})
	if err != nil {
		return nil, err
	}
	if resp.Warning != "" {
		fmt.Printf("agent: server warning: %s\n", resp.Warning)
	}
	return &resp.Volume, nil
}

// heartbeatPayload collects volume server statistics and reports storage
// usage against the configured quota: used = bytes actually stored by the
// agent, free = remaining quota (capacity - used). The node is marked
// degraded once the quota is exhausted.
func heartbeatPayload(sup *supervisor.Supervisor, dataDir string, capacityBytes int64) api.HeartbeatRequest {
	status := sup.FetchStatus(2 * time.Second)
	used := dirSize(dataDir)
	free := capacityBytes - used
	if free < 0 {
		free = 0
	}
	state := "online"
	if !status.Alive {
		state = "degraded"
	} else if used >= capacityBytes {
		state = "degraded" // storage quota exhausted
	}
	return api.HeartbeatRequest{
		Status:            state,
		DiskFreeBytes:     free,
		DiskUsedBytes:     used,
		VolumeServerAlive: status.Alive,
		VolumesCount:      status.VolumesCount,
		EcShardsCount:     status.EcShardsCount,
	}
}

func pickPort(serverPort, localPort int) int {
	if serverPort > 0 {
		return serverPort
	}
	if localPort > 0 {
		return localPort
	}
	return 8080
}

func pickRack(localRack, serverRack string) string {
	if localRack != "" {
		return localRack
	}
	return serverRack
}

func pickMax(localMax, serverMax int) int {
	if localMax > 0 {
		return localMax
	}
	return serverMax
}

func pickDisk(localDisk, serverDisk string) string {
	if localDisk != "" {
		return localDisk
	}
	return serverDisk
}

func abs(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	wd, _ := os.Getwd()
	return filepath.Join(wd, p)
}
