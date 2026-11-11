// Package mcpbridge serves a backend's tools.Registry to a vendor CLI run on a
// loopback MCP server and hosts the run's http stop hook on the same listener.
// ContextMatrix's own MCP server is not bridged: the CLI reaches it directly.
package mcpbridge
