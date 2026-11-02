package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

	st := &agentState{lastIP: ip, volCfg: volCfg, startedAt: time.Now()}

	// Slow statistics (volume server status, data dir size) are collected in
	// the background with caching: the heartbeat loop must never block on a
	// busy volume server or on a large data directory walk.
	cache := &statsCache{ready: make(chan struct{})}
	go statsLoop(ctx, cache, sup, dataDir)

	// Heartbeats run in their own goroutine: the agent proves it is alive on
	// schedule no matter what else it is doing (stat collection, restarts).
	go heartbeatLoop(ctx, client, cfg, sup, st, cache)

	// IP changes (DHCP renew) are watched separately from heartbeats.
	go ipWatchLoop(ctx, client, cfg, sup, st)

	<-ctx.Done()
	fmt.Println("agent: stopping")
	return sup.Stop()
}

// agentState is the mutable agent state shared between the background loops.
// startedAt is immutable (agent process start time, shown on the dashboard).
type agentState struct {
	mu        sync.Mutex
	lastIP    string
	volCfg    *api.VolumeConfig
	startedAt time.Time
}

func (st *agentState) snapshot() (ip string, volCfg *api.VolumeConfig) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.lastIP, st.volCfg
}

// statsCache caches slow-to-collect node statistics so heartbeat payloads are
// assembled instantly. The first completed refresh closes ready.
type statsCache struct {
	mu          sync.Mutex
	used        int64
	status      supervisor.Status
	restarts    int       // volume server process starts (supervisor counter)
	refreshedAt time.Time // when these statistics were collected
	logTail     string    // volume log tail, filled only when degraded
	ready       chan struct{}
}

// refresh recomputes the cached statistics. It may take seconds (volume
// status request, data dir walk): only call it from the background loop.
func (c *statsCache) refresh(dataDir string, sup *supervisor.Supervisor) {
	status := sup.FetchStatus(5 * time.Second)
	used := dirSize(dataDir)
	// The log tail is collected only while the volume server is down: it
	// explains WHY the node is degraded (disk full, io error, ...) and is
	// shown in the dashboard's node details.
	tail := ""
	if !status.Alive {
		tail = strings.Join(sup.LogTail(4), "\n")
	}
	c.mu.Lock()
	c.used = used
	c.status = status
	c.restarts = sup.RestartCount()
	c.refreshedAt = time.Now()
	c.logTail = tail
	c.mu.Unlock()
	select {
	case <-c.ready:
	default:
		close(c.ready)
	}
}

func (c *statsCache) get() (used int64, status supervisor.Status, restarts int, statsAge time.Duration, logTail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	age := time.Duration(0)
	if !c.refreshedAt.IsZero() {
		age = time.Since(c.refreshedAt)
	}
	return c.used, c.status, c.restarts, age, c.logTail
}

// statsLoop refreshes the statistics cache every 30 seconds in the background.
func statsLoop(ctx context.Context, cache *statsCache, sup *supervisor.Supervisor, dataDir string) {
	cache.refresh(dataDir, sup) // first sample right away
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cache.refresh(dataDir, sup)
		}
	}
}

// heartbeatLoop sends a heartbeat every interval, starting immediately.
func heartbeatLoop(ctx context.Context, client *api.Client, cfg *config.Config, sup *supervisor.Supervisor, st *agentState, cache *statsCache) {
	interval := time.Duration(cfg.HeartbeatIntervalSec) * time.Second
	if interval <= 0 {
		interval = 10 * time.Second
	}
	// Give the first statistics sample up to 5 seconds; the heartbeat must
	// not be held hostage by a slow volume server or a huge data directory.
	select {
	case <-cache.ready:
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
		return
	}
	sendHeartbeat(ctx, client, cfg, sup, st, cache)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sendHeartbeat(ctx, client, cfg, sup, st, cache)
		}
	}
}

// sendHeartbeat reports the cached statistics; on a heartbeat error it
// re-registers to recover (the control plane registry is in-memory).
func sendHeartbeat(ctx context.Context, client *api.Client, cfg *config.Config, sup *supervisor.Supervisor, st *agentState, cache *statsCache) {
	used, status, restarts, statsAge, logTail := cache.get()
	free := cfg.CapacityBytes - used
	if free < 0 {
		free = 0
	}
	state := "online"
	if !status.Alive {
		state = "degraded"
	} else if used >= cfg.CapacityBytes {
		state = "degraded" // storage quota exhausted
	}
	hb := api.HeartbeatRequest{
		Status:            state,
		DiskFreeBytes:     free,
		DiskUsedBytes:     used,
		VolumeServerAlive: status.Alive,
		VolumesCount:      status.VolumesCount,
		EcShardsCount:     status.EcShardsCount,

		AgentVersion:    agentVersion,
		WeedVersion:     status.WeedVersion,
		AgentUptimeSec:  int64(time.Since(st.startedAt).Seconds()),
		VolumeUptimeSec: int64(status.VolumeUptime.Seconds()),
		RestartsTotal:   restarts,
		StatsAgeSec:     int64(statsAge.Seconds()),
		LogTail:         logTail,
	}
	if err := client.Heartbeat(ctx, cfg.NodeID, hb); err != nil {
		fmt.Printf("agent: heartbeat error: %v\n", err)
		// The control plane keeps its node registry in memory and
		// forgets nodes on restart; re-register to recover.
		ip, _ := st.snapshot()
		newCfg, rerr := register(ctx, client, cfg, ip)
		if rerr != nil {
			fmt.Printf("agent: re-register failed: %v\n", rerr)
			return
		}
		applyVolumeConfig(sup, cfg, newCfg)
		fmt.Println("agent: re-registered after heartbeat error")
		return
	}
}

// applyVolumeConfig restarts the volume server if the new config differs.
func applyVolumeConfig(sup *supervisor.Supervisor, cfg *config.Config, volCfg *api.VolumeConfig) {
	volArgs := sup.Args()
	newPort := pickPort(volCfg.VolumePort, cfg.Port)
	newMax := pickMax(cfg.MaxVolumes, volCfg.MaxVolumes)
	newDisk := pickDisk(cfg.DiskType, volCfg.DiskType)
	newRack := pickRack(cfg.Rack, volCfg.Rack)
	if volArgs.Master != volCfg.Master || volArgs.Port != newPort ||
		volArgs.MaxVolumes != newMax || volArgs.DiskType != newDisk ||
		volArgs.Rack != newRack {
		volArgs.Master = volCfg.Master
		volArgs.Port = newPort
		volArgs.MaxVolumes = newMax
		volArgs.DiskType = newDisk
		volArgs.Rack = newRack
		if err := sup.Restart(volArgs, 2*time.Second); err != nil {
			fmt.Printf("agent: restart after re-register failed: %v\n", err)
		}
	}
}

// ipWatchLoop re-registers and restarts the volume server when the local IP
// changes (DHCP renew). Runs in its own goroutine so it never delays a
// heartbeat.
func ipWatchLoop(ctx context.Context, client *api.Client, cfg *config.Config, sup *supervisor.Supervisor, st *agentState) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			newIP, err := supervisor.DetectIP()
			if err != nil {
				continue
			}
			lastIP, _ := st.snapshot()
			if newIP == lastIP {
				continue
			}
			fmt.Printf("agent: IP changed %s -> %s, re-registering\n", lastIP, newIP)
			volCfg, err := register(ctx, client, cfg, newIP)
			if err != nil {
				fmt.Printf("agent: re-register failed: %v\n", err)
				continue
			}
			st.mu.Lock()
			st.lastIP = newIP
			st.volCfg = volCfg
			st.mu.Unlock()
			volArgs := sup.Args()
			volArgs.IP = newIP
			if err := sup.Restart(volArgs, 2*time.Second); err != nil {
				fmt.Printf("agent: restart after IP change failed: %v\n", err)
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
		OSVersion:     osVersion(),
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
