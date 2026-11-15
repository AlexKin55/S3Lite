package main

// agentVersion is reported in every heartbeat so the control plane dashboard
// can show which nodes run an outdated agent. It is overridden at build time
// via -X s3lite.dev/agent/cmd/agent.agentVersion=<version> so it always
// matches src/agent/VERSION (the packaging source of truth).
var agentVersion = "0.0.0"
