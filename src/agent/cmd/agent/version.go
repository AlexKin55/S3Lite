package main

// agentVersion is reported in every heartbeat so the control plane dashboard
// can show which nodes run an outdated agent. Keep it in sync with
// src/agent/VERSION (the packaging source of truth).
const agentVersion = "0.0.3"
