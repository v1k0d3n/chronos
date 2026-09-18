/*
Copyright 2026 Chronos project and its authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mcpserver

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Version is reported to clients during the MCP handshake.
const Version = "v1alpha1"

// serverName is how the server identifies itself in the MCP handshake.
const serverName = "chronos-mcp"

// changesInWindowDescription is what an agent reads when deciding whether to
// reach for this tool. It leads with the question the tool answers, because
// selection happens on this text alone.
const changesInWindowDescription = `List cluster changes recorded by Chronos within a time window.

Answers "what changed here, and who changed it, shortly before this problem
started" — usually the first question worth asking during an incident, and the
one that most often resolves it.

Each entry carries the verb, the target object, the actor and how confidently
they were identified, the changed fields, and an assessed risk level. Where
before and after snapshots exist, the entry includes the exact diff_snapshots
call to inspect them.

An empty result is a real finding: it means nothing in scope changed during that
window, which rules the platform out as the cause.`

// diffSnapshotsDescription likewise leads with the question.
const diffSnapshotsDescription = `Compare two Chronos ResourceSnapshots field by field.

Shows exactly what changed between two captured states of the same object.
Status and metadata churn are ignored, so what is returned is what a person
actually changed.

Values redacted at capture — Secret data, pull secrets, service account tokens —
are never reconstructed. Redacted paths are listed separately so that a change
to a withheld field is visible as having occurred without exposing its value.`

// NewMCPServer builds the MCP server and registers the Chronos tools.
func NewMCPServer(s *Server) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    serverName,
		Title:   "Chronos change timeline",
		Version: Version,
	}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "changes_in_window",
		Description: changesInWindowDescription,
		Annotations: &mcp.ToolAnnotations{
			// Chronos is an immutable ledger; nothing here can alter the cluster.
			// Declaring that lets a client treat these as safe to call freely.
			ReadOnlyHint: true,
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ChangesInWindowArgs) (*mcp.CallToolResult, any, error) {
		return textResult(s.ChangesInWindow(ctx, args))
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "diff_snapshots",
		Description: diffSnapshotsDescription,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args DiffSnapshotsArgs) (*mcp.CallToolResult, any, error) {
		return textResult(s.DiffSnapshots(ctx, args))
	})

	return srv
}

// textResult adapts a (string, error) tool into an MCP result.
//
// A failure is returned as an error result rather than a protocol error: the
// agent should see what went wrong and adapt — usually by widening a window or
// correcting a name — instead of having the call fail opaquely.
func textResult(out string, err error) (*mcp.CallToolResult, any, error) {
	if err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
		}, nil, nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: out}},
	}, nil, nil
}

// NewHTTPHandler returns a streamable-HTTP handler serving the Chronos tools,
// gated on a bearer token that the API server has authenticated.
//
// The middleware is not optional. Over a network listener the caller's identity
// is the only thing standing between a reader and the whole cluster's change
// history, so a request without a valid token is refused at the door with 401
// rather than being answered under the server's own identity.
func NewHTTPHandler(s *Server, verifier auth.TokenVerifier) http.Handler {
	srv := NewMCPServer(s)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	return auth.RequireBearerToken(verifier, nil)(mcpHandler)
}

// RunStdio serves the tools over stdio until the context is cancelled. Useful
// for pointing a local client such as an IDE at a cluster directly.
func RunStdio(ctx context.Context, s *Server) error {
	return NewMCPServer(s).Run(ctx, &mcp.StdioTransport{})
}
