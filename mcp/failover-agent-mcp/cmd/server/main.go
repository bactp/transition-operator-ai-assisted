package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/vitu1234/transition-operator/mcp/failover-agent-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	// Defaults to serving streamable HTTP on :8080 - this runs as a Kubernetes
	// Deployment via the MCPServer CR (httpTransport.targetPort: 8080), and
	// kmcp/kagent's generated Deployment does not inject a --http flag, so the
	// container's own default must already be "serve HTTP" (verified against
	// the skill-manager-mcp Deployment already running in this cluster, which
	// has no command/args override at all). Pass -stdio for local Claude
	// Desktop/`kmcp run local`-style stdio usage instead.
	httpAddr = flag.String("http", ":8080", "address to serve streamable HTTP MCP on")
	stdio    = flag.Bool("stdio", false, "serve MCP over stdin/stdout instead of HTTP")
)

func main() {
	flag.Parse()

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Create the MCP server
	server := mcp.NewServer(&mcp.Implementation{Name: "failover-agent-mcp", Version: "0.1.0"}, nil)

	// Register tools
	tools.AddToolsToServer(server)

	// Start server with appropriate transport
	if *stdio {
		t := mcp.NewLoggingTransport(mcp.NewStdioTransport(), os.Stderr)
		return server.Run(context.Background(), t)
	}

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil)
	log.Printf("MCP server listening at %s", *httpAddr)
	return http.ListenAndServe(*httpAddr, handler)
}
