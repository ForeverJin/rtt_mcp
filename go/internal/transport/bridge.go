package transport

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"rtt-mcp-server/internal/config"
)

// RunBridge connects to the shared daemon over SSE, discovers its tools, and
// exposes them over stdio MCP — forwarding every call to the daemon. The bridge
// never opens the probe; it is a pure proxy so two clients share one owner.
func RunBridge(ctx context.Context) error {
	cfg := config.Load()

	client := mcp.NewClient(impl(), nil)
	transport := &mcp.SSEClientTransport{
		Endpoint:   cfg.DaemonURL,
		HTTPClient: &http.Client{Transport: authTransport(cfg.AuthToken)},
	}
	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return fmt.Errorf("connect daemon: %w", err)
	}
	defer cs.Close()

	list, err := cs.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return fmt.Errorf("list daemon tools: %w", err)
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "rtt-bridge", Version: serverVersion}, nil)
	for _, t := range list.Tools {
		tool := t
		srv.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			res, err := cs.CallTool(ctx, &mcp.CallToolParams{
				Name:      tool.Name,
				Arguments: req.Params.Arguments,
			})
			if err != nil {
				return nil, err
			}
			return res, nil
		})
		fmt.Fprintf(os.Stderr, "[rtt-bridge] forwarding tool: %s\n", tool.Name)
	}
	return srv.Run(ctx, &mcp.StdioTransport{})
}
