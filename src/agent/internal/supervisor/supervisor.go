package supervisor

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// VolumeArgs is the weed volume launch configuration (assembled from the
// registration response and local agent settings).
type VolumeArgs struct {
	WeedBin    string // path to the weed binary
	IP         string // node address advertised in the cluster
	Port       int    // volume server HTTP port
	Master     string // master address
	Dir        string // data directory
	DataCenter string // -dataCenter
	Rack       string // -rack
	MaxVolumes int    // -max (0 = SeaweedFS default)
	DiskType   string // -disk
	LogPath    string // process log file (empty = discard)
}

// Status summarizes the volume server state for heartbeats.
type Status struct {
	Alive         bool
	VolumesCount  int
	EcShardsCount int
	WeedVersion   string        // reported by the volume server /status endpoint
	VolumeUptime  time.Duration // time since the current process was started
}

// Supervisor manages the weed volume process: start, watch, restart on crash.
type Supervisor struct {
	mu           sync.Mutex
	args         VolumeArgs
	cmd          *exec.Cmd
	stopCh       chan struct{}
	log          *os.File
	restartCount int
	lastStart    time.Time // when the current process was started
}

func New(args VolumeArgs) *Supervisor {
	return &Supervisor{args: args, stopCh: make(chan struct{})}
}

// Start launches weed volume and supervises it: on unexpected exit it is
// restarted after restartDelay with an exponential backoff.
func (s *Supervisor) Start(restartDelay time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.args.LogPath != "" {
		f, err := os.OpenFile(s.args.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		s.log = f
	}
	return s.startLocked(restartDelay)
}

func (s *Supervisor) startLocked(restartDelay time.Duration) error {
	args := []string{
		"volume",
		"-ip=" + s.args.IP,
		"-port=" + strconv.Itoa(s.args.Port),
		"-master=" + s.args.Master,
		"-dir=" + s.args.Dir,
	}
	if s.args.DataCenter != "" {
		args = append(args, "-dataCenter="+s.args.DataCenter)
	}
	if s.args.Rack != "" {
		args = append(args, "-rack="+s.args.Rack)
	}
	if s.args.MaxVolumes > 0 {
		args = append(args, "-max="+strconv.Itoa(s.args.MaxVolumes))
	}
	if s.args.DiskType != "" {
		args = append(args, "-disk="+s.args.DiskType)
	}

	cmd := exec.Command(s.args.WeedBin, args...)
	if s.log != nil {
		cmd.Stdout = s.log
		cmd.Stderr = s.log
	} else {
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start weed volume: %w", err)
	}
	s.cmd = cmd
	s.restartCount++
	s.lastStart = time.Now()

	go s.watch(restartDelay)
	return nil
}

// watch waits for the process to exit and restarts it if the exit was
// unexpected (i.e. not a graceful Stop).
func (s *Supervisor) watch(restartDelay time.Duration) {
	err := s.cmd.Wait()

	s.mu.Lock()
	isStopped := s.cmd == nil || s.stopChSignaled()
	s.cmd = nil
	s.mu.Unlock()

	if isStopped {
		return // stopped manually (graceful)
	}

	fmt.Printf("weed volume exited unexpectedly (err=%v); restarting in %s\n", err, restartDelay)
	time.Sleep(restartDelay)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopChSignaled() {
		return
	}
	// Double the backoff up to 30s so a crash loop does not spin.
	next := restartDelay
	if next < 30*time.Second {
		next *= 2
	}
	if err := s.startLocked(next); err != nil {
		fmt.Printf("restart weed volume failed: %v\n", err)
	}
}

func (s *Supervisor) stopChSignaled() bool {
	select {
	case <-s.stopCh:
		return true
	default:
		return false
	}
}

// Stop gracefully stops the process: SIGINT, wait, then SIGKILL.
func (s *Supervisor) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	close(s.stopCh)
	if s.cmd == nil {
		return nil
	}
	proc := s.cmd.Process
	if proc == nil {
		return nil
	}
	_ = proc.Signal(os.Interrupt) // weed volume handles SIGINT as graceful stop
	done := make(chan struct{})
	go func() { _ = s.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = proc.Kill()
		<-done
	}
	s.cmd = nil
	if s.log != nil {
		_ = s.log.Close()
		s.log = nil
	}
	return nil
}

// Restart stops the current process and starts a new one with new args.
func (s *Supervisor) Restart(args VolumeArgs, restartDelay time.Duration) error {
	if err := s.Stop(); err != nil {
		return err
	}
	s.mu.Lock()
	s.args = args
	s.mu.Unlock()
	return s.Start(restartDelay)
}

// RestartCount returns the number of process starts.
func (s *Supervisor) RestartCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restartCount
}

// Args returns the current volume launch arguments.
func (s *Supervisor) Args() VolumeArgs {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.args
}

// FetchStatus queries the volume server (GET /status) and returns a summary.
// The Volumes field covers both regular volumes and EC shards (each entry has
// an "EcShards" sub-list), so EcShardsCount is derived from the nested lists.
func (s *Supervisor) FetchStatus(timeout time.Duration) Status {
	s.mu.Lock()
	url := fmt.Sprintf("http://%s:%d/status", s.args.IP, s.args.Port)
	started := s.lastStart
	s.mu.Unlock()
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return Status{Alive: false}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Status{Alive: false}
	}
	var st struct {
		Version string `json:"Version"`
		Volumes []struct {
			EcShards []any `json:"EcShards"`
		} `json:"Volumes"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&st)
	ec := 0
	for _, v := range st.Volumes {
		ec += len(v.EcShards)
	}
	return Status{
		Alive:         true,
		VolumesCount:  len(st.Volumes),
		EcShardsCount: ec,
		WeedVersion:   st.Version,
		VolumeUptime:  time.Since(started),
	}
}

// LogTail returns the last lines of the volume server log file (at most
// maxLines). It reads only the tail of the file, so it is cheap even for a
// large log. Returns nil when the log is not configured or unreadable.
func (s *Supervisor) LogTail(maxLines int) []string {
	s.mu.Lock()
	path := s.args.LogPath
	s.mu.Unlock()
	if path == "" || maxLines <= 0 {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	const chunk = 8 * 1024 // read at most the last 8 KiB
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return nil
	}
	read := int64(chunk)
	if st.Size() < read {
		read = st.Size()
	}
	buf := make([]byte, read)
	if _, err := f.ReadAt(buf, st.Size()-read); err != nil {
		return nil
	}
	text := strings.TrimRight(string(buf), "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines
}

// DetectIP returns the first non-loopback IPv4 address of the machine.
func DetectIP() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipv4 := ipnet.IP.To4(); ipv4 != nil {
				return ipv4.String(), nil
			}
		}
	}
	return "", fmt.Errorf("no non-loopback IPv4 address found")
}
