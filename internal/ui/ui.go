// Package ui serves the interactive task board that hosts render inside the
// conversation.
//
// It implements the MCP Apps extension (spec 2026-01-26): the server advertises
// the ui extension capability, exposes an HTML document under the ui:// scheme,
// and points a tool at that document through _meta.ui.resourceUri. The host then
// renders the HTML in a sandboxed iframe and proxies the view's own tool calls
// back to this server — which is what lets the board keep refreshing after the
// tool call that opened it has already returned.
//
// Hosts that do not implement the extension ignore all of this and fall back to
// the tool's text result, so registering it costs nothing on those clients.
package ui

import _ "embed"

// ResourceURI addresses the board. It is both the resource's own URI and the
// value tools carry in _meta.ui.resourceUri.
const ResourceURI = "ui://cli-agent-mcp/task-board.html"

// MIMEType is the only content type the spec permits for a UI resource; other
// values are reserved for future extensions.
const MIMEType = "text/html;profile=mcp-app"

// ExtensionName is the capability key hosts look for to decide whether this
// server can render views.
const ExtensionName = "io.modelcontextprotocol/ui"

// BoardHTML is the whole view — markup, styles and script in a single file.
// It has to be self-contained: the host renders it under a deny-by-default CSP
// that blocks external origins, and this server declares no csp allowances.
//
//go:embed board.html
var BoardHTML string

// Capability is the settings object advertised under ExtensionName during
// initialize.
func Capability() map[string]any {
	return map[string]any{"mimeTypes": []string{MIMEType}}
}

// ToolMeta is the _meta payload that links a tool to the board, telling the host
// to render the view in place of the tool's text result.
func ToolMeta() map[string]any {
	return map[string]any{"ui": map[string]any{"resourceUri": ResourceURI}}
}

// ResourceMeta carries the board's own rendering preferences.
func ResourceMeta() map[string]any {
	return map[string]any{"ui": map[string]any{"prefersBorder": true}}
}

// ClientRenders reports whether a client declared, during initialize, that it
// can render views like the board.
//
// It is worth asking because the failure is silent by design: a host that does
// not implement the extension ignores the ui:// resource and shows the tool's
// text result instead, which reads as the board simply not being very good
// rather than never having been rendered at all.
func ClientRenders(extensions map[string]any) bool {
	if extensions == nil {
		return false
	}
	_, ok := extensions[ExtensionName]
	return ok
}
