package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const toolDescription = `Get Go documentation for a package, type, function, or method. 
This is the preferred and most efficient way to understand Go packages, providing official package 
documentation in a concise format. Use this before attempting to read source files directly. Results 
are cached and optimized for AI consumption.`

// Create a shared input schema definition to ensure consistency
var docInputSchema = mcp.ToolInputSchema{
	Type: "object",
	Properties: map[string]interface{}{
		"path": map[string]interface{}{
			"type":        "string",
			"description": "Path to the Go package or file. This should be the import path for packages or file path for local files.",
		},
		"target": map[string]interface{}{
			"type":        "string",
			"description": "Optional: Specific symbol to get documentation for (e.g., function name, type name, interface name). Leave empty to get full package documentation.",
		},
		"cmd_flags": map[string]interface{}{
			"type": "array",
			"items": map[string]interface{}{
				"type": "string",
			},
			"description": "Optional: Additional go doc command flags. Common flags:\n" +
				"  -all: Show all documentation for package\n" +
				"  -src: Show the source code\n" +
				"  -u: Show unexported symbols as well as exported",
		},
	},
	Required: []string{"path"},
}

type GodocServer struct {
	server *server.MCPServer
	cache  map[string]cachedDoc
}

type cachedDoc struct {
	content   string
	timestamp time.Time
	byteSize  int
}

// runGoDoc executes the go doc command with the given arguments
func (s *GodocServer) runGoDoc(args ...string) (string, error) {
	// Create cache key from args
	cacheKey := strings.Join(args, "|")

	// Check cache (with 5 minute expiration)
	if doc, ok := s.cache[cacheKey]; ok {
		if time.Since(doc.timestamp) < 5*time.Minute {
			log.Printf("Cache hit for %s (%d bytes)", cacheKey, doc.byteSize)
			return doc.content, nil
		}
	}

	cmd := exec.Command("go", append([]string{"doc"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("go doc error: %v\noutput: %s", err, string(out))
	}

	content := string(out)
	s.cache[cacheKey] = cachedDoc{
		content:   content,
		timestamp: time.Now(),
		byteSize:  len(content),
	}

	log.Printf("Cache miss for %s (%d bytes)", cacheKey, len(content))
	return content, nil
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
			"description": toolDescription,
			"inputSchema": docInputSchema,
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

	// Calculate byte size
	byteSize := len(doc)

	// Add metadata to help Claude understand the efficiency
	metadata := fmt.Sprintf(
		"Documentation retrieved efficiently (%d bytes) using official Go documentation tools. "+
			"This represents the canonical, structured documentation for the requested package/symbol.",
		byteSize,
	)

	// Create the result with metadata
	result := &mcp.CallToolResult{
		Content: []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": metadata + "\n\n" + doc,
			},
		},
	}

	log.Printf("Returning result (%d bytes)", byteSize)
	return result, nil
}

func main() {
	// Set up logging to stderr (since stdout is used for MCP communication)
	log.SetOutput(os.Stderr)
	log.Printf("Starting godoc-mcp server...")

	godocServer := &GodocServer{
		cache: make(map[string]cachedDoc),
	}

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
		Description: toolDescription,
		InputSchema: docInputSchema,
	}
	s.AddTool(tool, godocServer.handleToolCall)

	// Run server using stdio
	log.Printf("Starting stdio server...")
	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}