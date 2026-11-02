package main

import (
	"bytes"
	"context"
	_ "embed" // reinstall_agent.sh is embedded into the binary
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// reinstallScript is the agent self-update script. It is embedded (not
// downloaded) so the control plane never ships executable code to the node:
// the server only sends a one-word trigger ("update_agent") and the agent
// runs this locally-audited script. The new agent binary itself comes from
// the GPG-signed package repository via the package manager.
//
//go:embed reinstall_agent.sh
var reinstallScript string

const (
	// selfUpdatePackage is the package the self-update reinstalls.
	selfUpdatePackage = "s3lite-agent"
	// selfUpdateTimeout bounds the whole update (package download included).
	selfUpdateTimeout = 15 * time.Minute
	// selfUpdateLogName is the update script log inside the agent dir
	// (direct/root mode; the helper unit logs to /var/lib/s3lite/agent/update.log).
	selfUpdateLogName = "agent-update.log"
	// selfUpdateResultMaxAge bounds how old a consumed result marker may be.
	selfUpdateResultMaxAge = 10 * time.Minute

	// Privileged helper paths, shipped by the s3lite-agent RPM. The agent
	// runs as the unprivileged "s3lite" user and cannot install packages
	// itself; it only drops a request file that the root
	// s3lite-agent-update.path unit watches (systemd performs the
	// privileged action — no D-Bus, no polkit, no sudo, no setuid).
	helperScript = "/usr/lib/s3lite/agent-update.sh"
	helperDir    = "/var/lib/s3lite/agent"
	helperLog    = helperDir + "/update.log"
	helperReq    = helperDir + "/update-request"
	helperResult = helperDir + "/update-result"
)

// updating guards against concurrent self-updates: a repeated update_agent
// command while one is already in progress is ignored.
var updating atomic.Bool

// updateResult is the parsed update-result marker written by the update
// script (see reinstall_agent.sh write_update_marker).
type updateResult struct {
	status  string // "done" | "failed"
	version string
	detail  string
	ts      int64 // unix seconds
}

// parseUpdateResult parses KEY=VALUE lines, last occurrence wins.
func parseUpdateResult(data string) *updateResult {
	kv := make(map[string]string)
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, "="); i > 0 {
			kv[line[:i]] = line[i+1:]
		}
	}
	status := kv["STATUS"]
	if status != "done" && status != "failed" {
		return nil
	}
	ts, _ := strconv.ParseInt(kv["TS"], 10, 64)
	return &updateResult{
		status:  status,
		version: kv["VERSION"],
		detail:  kv["DETAIL"],
		ts:      ts,
	}
}

// consumeUpdateResult is called at agent startup: when a self-update finished
// while this process was being replaced (the helper unit restarts
// s3lite-agent.service, killing the requesting agent), the fresh agent
// reports the outcome to the control plane. The marker is consumed once.
func consumeUpdateResult(st *agentState) {
	data, err := os.ReadFile(helperResult)
	if err != nil {
		return
	}
	res := parseUpdateResult(string(data))
	if res == nil {
		return
	}
	_ = os.Remove(helperResult) //nolint:errcheck // consumed once; best effort
	age := time.Since(time.Unix(res.ts, 0))
	if age > selfUpdateResultMaxAge || age < -time.Minute {
		fmt.Printf("agent: ignoring stale self-update result marker (age %s)\n", age)
		return
	}
	fmt.Printf("agent: previous self-update finished: %s (version %s)\n", res.status, res.version)
	if res.status == "done" {
		st.setUpdate("done", "agent reinstalled (version "+res.version+") and restarted")
		return
	}
	detail := res.detail
	if detail == "" {
		detail = "self-update failed"
	}
	st.setUpdate("failed", detail)
}

// triggerSelfUpdate arms the "running" state and starts the update in the
// background. It is called from the heartbeat loop when the control plane
// delivers an update_agent command. Returns false if the update was not
// started (already running, unsupported OS).
func triggerSelfUpdate(ctx context.Context, agentDir string, st *agentState) bool {
	if runtime.GOOS != "linux" {
		st.setUpdate("failed", "self-update is supported on Linux only")
		return false
	}
	if !updating.CompareAndSwap(false, true) {
		fmt.Println("agent: self-update already in progress, ignoring")
		return false
	}
	// The status is reported to the control plane with every heartbeat while
	// the update runs; "done"/"failed" are cleared once acknowledged.
	st.setUpdate("running", "")
	go selfUpdate(ctx, agentDir, st)
	return true
}

// selfUpdate reinstalls the s3lite-agent package preserving configuration
// and restarts the agent service. Two modes:
//
//   - agent runs as root (dev environments, containers): run the embedded
//     script directly and parse its output;
//   - agent runs as the unprivileged "s3lite" user (packaged install): drop
//     the request file watched by the root s3lite-agent-update.path unit;
//     the root helper reinstalls the package and restarts the agent service.
//     The fresh agent process consumes the result marker at startup and
//     reports the outcome; this process only reports failures (on success it
//     is replaced by the restart before the marker appears).
func selfUpdate(ctx context.Context, agentDir string, st *agentState) {
	defer updating.Store(false)

	ctx, cancel := context.WithTimeout(ctx, selfUpdateTimeout)
	defer cancel()

	if os.Geteuid() == 0 {
		selfUpdateDirect(ctx, agentDir, st)
		return
	}
	selfUpdateSystemd(ctx, st)
}

// selfUpdateDirect runs the embedded script as root and reports the outcome
// parsed from its output.
func selfUpdateDirect(ctx context.Context, agentDir string, st *agentState) {
	logPath := filepath.Join(agentDir, selfUpdateLogName)
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		st.setUpdate("failed", "cannot open update log "+logPath+": "+err.Error())
		return
	}
	defer logf.Close() //nolint:errcheck // best effort
	fmt.Fprintf(logf, "agent self-update started %s\n", time.Now().Format(time.RFC3339))

	// Materialize the embedded script to /tmp: the package install may
	// replace the agent binary while the script is running.
	scriptPath := filepath.Join(os.TempDir(), "s3lite-agent-selfupdate.sh")
	if err := os.WriteFile(scriptPath, []byte(reinstallScript), 0o700); err != nil { //nolint:gosec // fixed name in /tmp
		st.setUpdate("failed", "cannot write update script: "+err.Error())
		return
	}
	defer os.Remove(scriptPath) //nolint:errcheck // best effort cleanup

	fmt.Printf("agent: self-update started (package %s), log: %s\n", selfUpdatePackage, logPath)

	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, "bash", scriptPath, selfUpdatePackage)
	cmd.Stdout = io.MultiWriter(os.Stdout, &buf)
	cmd.Stderr = io.MultiWriter(os.Stderr, &buf)
	err = cmd.Run()
	_, _ = logf.Write(buf.Bytes())

	kv := parseKV(buf.String())
	detail := kv["DETAIL"]
	if err != nil || kv["RESULT"] != "OK" {
		if detail == "" {
			detail = fmt.Sprintf("update script failed (exit %v)", err)
		}
		st.setUpdate("failed", detail)
		return
	}
	if kv["ACTIVE"] == "no" {
		if detail == "" {
			detail = "service not active after restart"
		}
		st.setUpdate("failed", detail)
		return
	}
	st.setUpdate("done", "agent reinstalled (version "+kv["NEW_VERSION"]+") and restarted")
}

// selfUpdateSystemd requests the privileged helper via the path unit and
// waits for the result marker.
func selfUpdateSystemd(ctx context.Context, st *agentState) {
	if _, err := os.Stat(helperScript); err != nil {
		st.setUpdate("failed",
			"agent is not installed from the s3lite-agent package (no update helper at "+helperScript+"); install the package or run the agent as root")
		return
	}
	startedAt := time.Now().Unix()
	// Drop a stale marker so the poll below cannot pick up an old result.
	_ = os.Remove(helperResult) //nolint:errcheck // best effort
	if err := os.MkdirAll(helperDir, 0o750); err != nil {
		st.setUpdate("failed", "cannot prepare the update request dir: "+err.Error())
		return
	}
	// The root s3lite-agent-update.path unit watches this file (inotify) and
	// starts the root helper unit — the agent itself makes no privileged
	// calls. ExecStartPre of the helper removes the request file.
	if err := os.WriteFile(helperReq, []byte(fmt.Sprintf("ts=%d\n", startedAt)), 0o644); err != nil {
		st.setUpdate("failed", "cannot write the update request "+helperReq+": "+err.Error())
		return
	}
	fmt.Printf("agent: self-update requested (package %s), helper log: %s\n", selfUpdatePackage, helperLog)

	// Wait for the result marker. On success the helper restarts
	// s3lite-agent.service, which replaces this process — the fresh agent
	// consumes the marker (consumeUpdateResult) and reports "done". This
	// poll therefore only completes for failures and timeouts.
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		data, err := os.ReadFile(helperResult)
		if err != nil {
			continue
		}
		res := parseUpdateResult(string(data))
		if res == nil || res.ts < startedAt {
			continue
		}
		_ = os.Remove(helperResult) //nolint:errcheck // consumed once
		detail := res.detail
		if res.status == "done" {
			st.setUpdate("done", "agent reinstalled (version "+res.version+") and restarted")
		} else {
			if detail == "" {
				detail = "self-update failed"
			}
			st.setUpdate("failed", detail)
		}
		return
	}
}

// parseKV extracts KEY=VALUE lines from script output (last wins).
func parseKV(s string) map[string]string {
	kv := make(map[string]string)
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, "="); i > 0 {
			kv[line[:i]] = line[i+1:]
		}
	}
	return kv
}
