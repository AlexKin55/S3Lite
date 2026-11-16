package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
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
	server := flag.String("server", "", "control plane address (scheme + host:port)")
	weedBin := flag.String("weed", "", "path to the SeaweedFS weed binary")
	storage := flag.String("storage", "", "agent data directory")
	capacityGB := flag.Int("capacityGB", 0, "node capacity in gigabytes")
	insecure := flag.Bool("insecure", false, "skip TLS certificate verification")
	flag.Parse()

	cfg, err := config.Load(*agentDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent: load config: %v\n", err)
		os.Exit(1)
	}
	// CLI flags override values from /etc/s3lite/agent.env.
	if *server != "" {
		cfg.Server = *server
	}
	if *weedBin != "" {
		cfg.WeedBin = *weedBin
	}
	if *storage != "" {
		cfg.DataDir = *storage
	}
	if *capacityGB > 0 {
		cfg.CapacityBytes = int64(*capacityGB) * 1024 * 1024 * 1024
	}
	if *insecure {
		cfg.TLSInsecure = true
	}

	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "agent: %v\n", err)
		os.Exit(1)
	}
}

func run(cfg *config.Config) error {
	agentDir := abs(cfg.DataDir)
	if agentDir == "" {
		agentDir = abs(".cluster/agent")
	}
	agentDir = filepath.Dir(agentDir)

	if cfg.WeedBin == "" {
		return fmt.Errorf("weed binary path is empty: set WEED_BIN in /etc/s3lite/agent.env")
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
		return fmt.Errorf("create data dir %s: %w (check permissions — the s3lite user must be able to read and write it)", dataDir, err)
	}
	// Verify the data directory is readable and writable by this process.
	testFile := filepath.Join(dataDir, ".s3lite-write-test")
	if err := os.WriteFile(testFile, []byte("ok"), 0o644); err != nil {
		return fmt.Errorf("data dir %s is not writable: %w (check permissions — the s3lite user must be able to read and write it)", dataDir, err)
	}
	os.Remove(testFile)
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

	// Derive max volumes from node capacity, EC slot ratio, and config limits.
	// totalSlots   = STORAGE_SIZE / volumeSizeLimitMB
	// ecSlots      = totalSlots * EC_SHARD_SLOT_RATIO / 100 (reserved for EC shards)
	// volumeSlots  = totalSlots - ecSlots
	// maxVolumes   = min(volumeSlots, user_MAX_VOLUMES if set, server_MAX_VOLUMES if set)
	var maxVolumes int
	if volCfg.VolumeSizeLimitMB > 0 && cfg.CapacityBytes > 0 {
		totalSlots := int(cfg.CapacityBytes / (int64(volCfg.VolumeSizeLimitMB) * 1024 * 1024))
		if totalSlots > 0 {
			ecSlots := totalSlots * cfg.ECShardSlotRatio / 100
			if ecSlots >= totalSlots {
				ecSlots = totalSlots - 1
			}
			if ecSlots < 0 {
				ecSlots = 0
			}
			volumeSlots := totalSlots - ecSlots
			// Cap by user-configured MAX_VOLUMES if set.
			if cfg.MaxVolumes > 0 && cfg.MaxVolumes < volumeSlots {
				volumeSlots = cfg.MaxVolumes
			}
			// Cap by server-configured MAX_VOLUMES if set.
			if volCfg.MaxVolumes > 0 && volCfg.MaxVolumes < volumeSlots {
				volumeSlots = volCfg.MaxVolumes
			}
			maxVolumes = volumeSlots
		} else {
			maxVolumes = 1
			fmt.Printf("agent: capacity %d bytes is below one volume (%d MB); granting 1 volume slot\n",
				cfg.CapacityBytes, volCfg.VolumeSizeLimitMB)
		}
	} else {
		// No capacity/volume size info: fall back to config limits.
		maxVolumes = pickMax(cfg.MaxVolumes, volCfg.MaxVolumes)
	}

	sup := supervisor.New(supervisor.VolumeArgs{
		WeedBin:    cfg.WeedBin,
		IP:         ip,
		Port:       volumePort,
		Master:     volCfg.Master,
		Dir:        dataDir,
		DataCenter: volCfg.DataCenter,
		Rack:       pickRack(cfg.Rack, volCfg.Rack),
		MaxVolumes: maxVolumes,
		DiskType:   pickDisk(cfg.DiskType, volCfg.DiskType),
		StorageGB:  int(cfg.CapacityBytes / (1024 * 1024 * 1024)),
		LogPath:    logPath,
		ECShardSlotRatio: cfg.ECShardSlotRatio,
	})

	if err := sup.Start(2 * time.Second); err != nil {
		return err
	}
	fmt.Printf("volume server started on %s:%d (master %s)\n", ip, volumePort, volCfg.Master)

	st := &agentState{lastIP: ip, volCfg: volCfg, startedAt: time.Now()}
	// If a previous self-update finished while the agent process was being
	// replaced (the update helper restarts the service), report its outcome.
	consumeUpdateResult(st)

	// Slow statistics (volume server status, data dir size) are collected in
	// the background with caching: the heartbeat loop must never block on a
	// busy volume server or on a large data directory walk.
	cache := &statsCache{ready: make(chan struct{})}
	go statsLoop(ctx, cache, sup, dataDir)

	// Heartbeats run in their own goroutine: the agent proves it is alive on
	// schedule no matter what else it is doing (stat collection, restarts).
	go heartbeatLoop(ctx, client, cfg, sup, st, cache, agentDir)

	// IP changes (DHCP renew) are watched separately from heartbeats.
	go ipWatchLoop(ctx, client, cfg, sup, st)

	<-ctx.Done()
	fmt.Println("agent: stopping")
	return sup.Stop()
}

// agentState is the mutable agent state shared between the background loops.
// startedAt is immutable (agent process start time, shown on the dashboard).
// updateStatus/updateDetail carry the agent self-update progress; they are
// reported with every heartbeat ("running") until the outcome ("done" /
// "failed") is acknowledged by the control plane and cleared (one-shot).
// restartStatus/restartDetail carry the agent restart progress the same way.
type agentState struct {
	mu           sync.Mutex
	lastIP       string
	volCfg       *api.VolumeConfig
	startedAt    time.Time
	updateStatus string
	updateDetail string
	restartStatus string
	restartDetail string
}

func (st *agentState) snapshot() (ip string, volCfg *api.VolumeConfig) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.lastIP, st.volCfg
}

func (st *agentState) setUpdate(status, detail string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.updateStatus, st.updateDetail = status, detail
}

func (st *agentState) updateSnapshot() (status, detail string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.updateStatus, st.updateDetail
}

func (st *agentState) setRestart(status, detail string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.restartStatus, st.restartDetail = status, detail
}

func (st *agentState) restartSnapshot() (status, detail string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.restartStatus, st.restartDetail
}

// statsCache caches slow-to-collect node statistics so heartbeat payloads are
// assembled instantly. The first completed refresh closes ready.
type statsCache struct {
	mu          sync.Mutex
	used        int64
	status      supervisor.Status
	restarts    int       // volume server process starts (supervisor counter)
	cpuLoad     int       // most recent CPU load percent
	prevTotal   uint64    // previous /proc/stat total time (for delta)
	prevIdle    uint64    // previous /proc/stat idle time (for delta)
	refreshedAt time.Time // when these statistics were collected
	logTail     string    // volume log tail, filled only when degraded
	ready       chan struct{}
}

// refresh recomputes the cached statistics. It may take seconds (volume
// status request, data dir walk): only call it from the background loop.
func (c *statsCache) refresh(dataDir string, sup *supervisor.Supervisor) {
	status := sup.FetchStatus(5 * time.Second)
	used := dirSize(dataDir)
	cpu := readCpuLoadDelta(&c.prevTotal, &c.prevIdle)
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
	c.cpuLoad = cpu
	c.refreshedAt = time.Now()
	c.logTail = tail
	c.mu.Unlock()
	select {
	case <-c.ready:
	default:
		close(c.ready)
	}
}

func (c *statsCache) get() (used int64, status supervisor.Status, restarts int, cpuLoad int, statsAge time.Duration, logTail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	age := time.Duration(0)
	if !c.refreshedAt.IsZero() {
		age = time.Since(c.refreshedAt)
	}
	return c.used, c.status, c.restarts, c.cpuLoad, age, c.logTail
}

// statsLoop refreshes the statistics cache every 5 seconds in the background.
// A 5-second interval lets the control plane detect a dead or hung volume
// server quickly, well before the heartbeat TTL expires.
func statsLoop(ctx context.Context, cache *statsCache, sup *supervisor.Supervisor, dataDir string) {
	cache.refresh(dataDir, sup) // first sample right away
	ticker := time.NewTicker(5 * time.Second)
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
func heartbeatLoop(ctx context.Context, client *api.Client, cfg *config.Config, sup *supervisor.Supervisor, st *agentState, cache *statsCache, agentDir string) {
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
	sendHeartbeat(ctx, client, cfg, sup, st, cache, agentDir)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sendHeartbeat(ctx, client, cfg, sup, st, cache, agentDir)
		}
	}
}

// sendHeartbeat reports the cached statistics; on a heartbeat error it
// re-registers to recover (the control plane registry is in-memory).
func sendHeartbeat(ctx context.Context, client *api.Client, cfg *config.Config, sup *supervisor.Supervisor, st *agentState, cache *statsCache, agentDir string) {
	used, status, restarts, cpuLoad, statsAge, logTail := cache.get()
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
		CpuLoad:           cpuLoad,

		AgentVersion:    agentVersion,
		WeedVersion:     status.WeedVersion,
		AgentUptimeSec:  int64(time.Since(st.startedAt).Seconds()),
		VolumeUptimeSec: int64(status.VolumeUptime.Seconds()),
		RestartsTotal:   restarts,
		StatsAgeSec:     int64(statsAge.Seconds()),
		LogTail:         logTail,
	}
	// Agent self-update progress: "running" is reported with every heartbeat
	// while the update is in progress; the final "done"/"failed" status is
	// one-shot and cleared once the control plane has acknowledged it.
	if updStatus, updDetail := st.updateSnapshot(); updStatus != "" {
		hb.UpdateStatus, hb.UpdateDetail = updStatus, updDetail
	}
	// Agent restart progress: "running" is reported with every heartbeat
	// while the restart is in progress; the final "done"/"failed" status is
	// one-shot and cleared once the control plane has acknowledged it.
	if rstStatus, rstDetail := st.restartSnapshot(); rstStatus != "" {
		hb.RestartStatus, hb.RestartDetail = rstStatus, rstDetail
	}
	resp, err := client.Heartbeat(ctx, cfg.NodeID, hb)
	if err != nil {
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

	// One-shot update statuses ("done"/"failed") are cleared as soon as the
	// control plane acknowledged them; "running" persists until the update
	// finishes (the agent process is then replaced by the service restart).
	if s, _ := st.updateSnapshot(); s == "done" || s == "failed" {
		st.setUpdate("", "")
	}
	// One-shot restart statuses ("done"/"failed") are cleared the same way.
	if s, _ := st.restartSnapshot(); s == "done" || s == "failed" {
		st.setRestart("", "")
	}

	// Control-plane commands delivered with this heartbeat. "update_agent"
	// reinstalls the agent package preserving the node configuration and
	// restarts the agent service (selfupdate.go). "restart_agent" restarts
	// the agent and the volume server in place.
	for _, cmd := range resp.Commands {
		if cmd == "update_agent" {
			triggerSelfUpdate(ctx, agentDir, st)
		}
		if cmd == "restart_agent" {
			triggerRestart(ctx, sup, st)
		}
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

// readCpuLoadDelta reads /proc/stat and returns CPU load percent since the
// previous call, using prevTotal/prevIdle for delta calculation. Returns 0 on
// the first call (no baseline yet) or on any read error.
func readCpuLoadDelta(prevTotal, prevIdle *uint64) int {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0
	}
	line := sc.Text()
	// First line: "cpu  user nice system idle iowait irq softirq steal guest guest_nice"
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0
	}

	var total, idle uint64
	for i := 1; i < len(fields); i++ {
		v, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return 0
		}
		total += v
		if i == 4 { // idle is the 4th value after "cpu"
			idle = v
		}
	}

	if *prevTotal == 0 {
		*prevTotal = total
		*prevIdle = idle
		return 0
	}

	dTotal := total - *prevTotal
	dIdle := idle - *prevIdle
	*prevTotal = total
	*prevIdle = idle

	if dTotal == 0 {
		return 0
	}
	return int((100 * (dTotal - dIdle)) / dTotal)
}
