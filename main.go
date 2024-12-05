package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type GodocServer struct {
	server *server.MCPServer
}

// runGoDoc executes the go doc command with the given arguments
func (s *GodocServer) runGoDoc(args ...string) (string, error) {
	cmd := exec.Command("go", append([]string{"doc"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("go doc error: %v\noutput: %s", err, string(out))
	}
	return string(out), nil
}

// validatePath ensures the path exists and is within allowed directories
func (s *GodocServer) validatePath(path string) error {
	// Convert to absolute path
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("invalid path: %v", err)
	}

	// Check if path exists
	info, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("path does not exist: %v", err)
	}

	// Check if it's a directory containing Go files
	if info.IsDir() {
		// Check for go.mod or .go files
		hasGoFiles := false
		entries, err := os.ReadDir(absPath)
		if err != nil {
			return fmt.Errorf("cannot read directory: %v", err)
		}
		for _, entry := range entries {
			if !entry.IsDir() && (entry.Name() == "go.mod" || strings.HasSuffix(entry.Name(), ".go")) {
				hasGoFiles = true
				break
			}
		}
		if !hasGoFiles {
			return fmt.Errorf("directory does not contain Go files")
		}
	} else if !strings.HasSuffix(absPath, ".go") {
		return fmt.Errorf("file is not a Go source file")
	}

	return nil
}

// handleListTools implements the tools/list endpoint
func (s *GodocServer) handleListTools() ([]interface{}, error) {
	log.Printf("handleListTools called")
	tools := []interface{}{
		map[string]interface{}{
			"name":        "get_doc",
			"description": "Get Go documentation for a package, type, function, or method",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "Path to the Go package or file",
					},
					"target": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Specific symbol to get documentation for (e.g., function name, type name)",
					},
					"cmd_flags": map[string]interface{}{
						"type": "array",
						"items": map[string]interface{}{
							"type": "string",
						},
						"description": "Optional: Additional go doc command flags (e.g., -all, -src, -u)",
					},
				},
				"required": []string{"path"},
			},
		},
	}
	log.Printf("Returning tools: %+v", tools)
	return tools, nil
}

// handleToolCall implements the tools/call endpoint
func (s *GodocServer) handleToolCall(arguments map[string]interface{}) (*mcp.CallToolResult, error) {
	log.Printf("handleToolCall called with arguments: %+v", arguments)

	// Extract the path from arguments
	path, ok := arguments["path"].(string)
	if !ok {
		return nil, fmt.Errorf("path argument is required")
	}

	if err := s.validatePath(path); err != nil {
		return nil, err
	}

	// Build command arguments
	var cmdArgs []string

	// Add any provided command flags
	if flags, ok := arguments["cmd_flags"].([]interface{}); ok {
		for _, flag := range flags {
			if flagStr, ok := flag.(string); ok {
				cmdArgs = append(cmdArgs, flagStr)
			}
		}
	}

	// Add the path
	cmdArgs = append(cmdArgs, path)

	// Add specific target if provided
	if target, ok := arguments["target"].(string); ok && target != "" {
		cmdArgs = append(cmdArgs, target)
	}

	// Run go doc command
	doc, err := s.runGoDoc(cmdArgs...)
	if err != nil {
		log.Printf("Error running go doc: %v", err)
		return nil, err
	}

	// Create the result
	result := &mcp.CallToolResult{
		Content: []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": doc,
			},
		},
	}
	log.Printf("Returning result: %+v", result)
	return result, nil
}

func main() {
	// Set up logging to stderr (since stdout is used for MCP communication)
	log.SetOutput(os.Stderr)
	log.Printf("Starting godoc-mcp server...")

	godocServer := &GodocServer{}

	// Create new MCP server with tools enabled
	s := server.NewMCPServer(
		"godoc-mcp",
		"0.1.0",
		server.WithToolCapabilities(true), // Enable tools
		server.WithLogging(),              // Add logging
	)
	godocServer.server = s

	log.Printf("Adding get_doc tool...")
	tool := mcp.Tool{
		Name:        "get_doc",
		Description: "Get Go documentation for a package, type, function, or method",
		InputSchema: mcp.ToolInputSchema{
			Type: "object",
			Properties: map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Path to the Go package or file",
				},
				"target": map[string]interface{}{
					"type":        "string",
					"description": "Optional: Specific symbol to get documentation for (e.g., function name, type name)",
				},
				"cmd_flags": map[string]interface{}{
					"type": "array",
					"items": map[string]interface{}{
						"type": "string",
					},
					"description": "Optional: Additional go doc command flags (e.g., -all, -src, -u)",
				},
			},
			Required: []string{"path"},
		},
	}
	s.AddTool(tool, godocServer.handleToolCall)

	// Run server using stdio
	log.Printf("Starting stdio server...")
	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
