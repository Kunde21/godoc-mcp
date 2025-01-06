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
are cached and optimized for AI consumption.

Best Practices:
1. ALWAYS try this tool first before reading package source code
2. Start with basic package documentation before looking at source code or specific symbols
3. Use -all flag when you need comprehensive package documentation
4. Only look up specific symbols after understanding the package overview

Common Usage Patterns:
- Standard library: Use just the package name (e.g., "io", "net/http")
- External packages: Use full import path (e.g., "github.com/user/repo")
- Local packages: Use relative path (e.g., "./pkg") or absolute path

The documentation is cached for 5 minutes to improve performance.`

// Create a shared input schema definition to ensure consistency
var docInputSchema = mcp.ToolInputSchema{
	Type: "object",
	Properties: map[string]interface{}{
		"path": map[string]interface{}{
			"type":        "string",
			"description": "Path to the Go package or file. This can be an import path (e.g., 'io', 'github.com/user/repo') or a local file path.",
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
		"working_dir": map[string]interface{}{
			"type":        "string",
			"description": "Optional: Working directory to execute go doc from. Required for some import paths that depend on module context.",
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

// runGoDoc executes the go doc command with the given arguments and optional working directory
func (s *GodocServer) runGoDoc(workingDir string, args ...string) (string, error) {
	// Create cache key that includes working directory
	cacheKey := workingDir + "|" + strings.Join(args, "|")

	// Check cache (with 5 minute expiration)
	if doc, ok := s.cache[cacheKey]; ok {
		if time.Since(doc.timestamp) < 5*time.Minute {
			log.Printf("Cache hit for %s (%d bytes)", cacheKey, doc.byteSize)
			return doc.content, nil
		}
	}

	cmd := exec.Command("go", append([]string{"doc"}, args...)...)
	if workingDir != "" {
		cmd.Dir = workingDir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Enhanced error handling with suggestions
		errStr := string(out)
		if strings.Contains(errStr, "no such package") || strings.Contains(errStr, "is not in std") {
			return "", fmt.Errorf("Package not found. Suggestions:\n"+
				"1. For standard library packages, use just the package name (e.g., 'io', 'net/http')\n"+
				"2. For external packages, ensure they are imported in the module\n"+
				"3. For local packages, provide the relative path (e.g., './pkg') or absolute path\n"+
				"4. Check for typos in the package name\n"+
				"Error details: %s", errStr)
		}
		if strings.Contains(errStr, "no such symbol") {
			return "", fmt.Errorf("Symbol not found. Suggestions:\n"+
				"1. Check if the symbol name is correct (case-sensitive)\n"+
				"2. Use -u flag to see unexported symbols\n"+
				"3. Use -all flag to see all package documentation\n"+
				"Error: %v", err)
		}
		if strings.Contains(errStr, "build constraints exclude all Go files") {
			return "", fmt.Errorf("No Go files found for current platform. Suggestions:\n"+
				"1. Try using -all flag to see all package files\n"+
				"2. Check if you need to set GOOS/GOARCH environment variables\n"+
				"Error: %v", err)
		}
		return "", fmt.Errorf("go doc error: %v\noutput: %s\nTip: Use -h flag to see all available options", err, errStr)
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

// validatePath ensures the path is either a valid local file/directory or appears to be a valid import path
func (s *GodocServer) validatePath(path string) (error, []string) {
	// If it starts with . or /, treat as local path and validate it exists
	if strings.HasPrefix(path, ".") || strings.HasPrefix(path, "/") {
		absPath, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("invalid path: %v", err), nil
		}

		info, err := os.Stat(absPath)
		if err != nil {
			return fmt.Errorf("path does not exist: %v", err), nil
		}

		if info.IsDir() {
			entries, err := os.ReadDir(absPath)
			if err != nil {
				return fmt.Errorf("cannot read directory: %v", err), nil
			}

			// Look for go files in current directory
			hasGoFiles := false
			for _, entry := range entries {
				if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") {
					hasGoFiles = true
					break
				}
			}

			// If no Go files found, collect subdirectories
			if !hasGoFiles {
				var subDirs []string
				for _, entry := range entries {
					if entry.IsDir() {
						// Check if subdirectory has any .go files
						subEntries, err := os.ReadDir(filepath.Join(absPath, entry.Name()))
						if err == nil {
							for _, subEntry := range subEntries {
								if !subEntry.IsDir() && strings.HasSuffix(subEntry.Name(), ".go") {
									subDirs = append(subDirs, filepath.Join(path, entry.Name()))
									break
								}
							}
						}
					}
				}
				if len(subDirs) > 0 {
					return fmt.Errorf("no Go files in directory, but found Go packages in subdirectories"), subDirs
				}
				return fmt.Errorf("no Go files found in directory or subdirectories"), nil
			}
		} else if !strings.HasSuffix(absPath, ".go") {
			return fmt.Errorf("file is not a Go source file"), nil
		}
		return nil, nil
	}

	// For all other paths, treat as import path
	return nil, nil
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

	err, subDirs := s.validatePath(path)
	if err != nil {
		if subDirs != nil {
			// Return a special response indicating available subdirectories
			metadata := fmt.Sprintf("No Go files found in %s, but found Go packages in the following subdirectories:\n", path)
			listing := strings.Join(subDirs, "\n")
			return &mcp.CallToolResult{
				Content: []interface{}{
					map[string]interface{}{
						"type": "text",
						"text": metadata + listing,
					},
				},
			}, nil
		}
		return nil, err
	}

	// Get working directory if provided
	workingDir := ""
	if wd, ok := arguments["working_dir"].(string); ok && wd != "" {
		if info, err := os.Stat(wd); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("invalid working directory: %v", err)
		}
		workingDir = wd
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

	// Run go doc command with working directory
	doc, err := s.runGoDoc(workingDir, cmdArgs...)
	if err != nil {
		log.Printf("Error running go doc: %v", err)
		return nil, err
	}

	// Calculate byte size
	byteSize := len(doc)

	// Enhanced metadata with usage guidance
	metadata := fmt.Sprintf(
		"Documentation retrieved efficiently (%d bytes) using official Go documentation tools.\n"+
			"This represents the canonical, structured documentation for the requested package/symbol.\n\n"+
			"Usage Tips:\n"+
			"1. Start with basic package docs before looking at source (-src flag)\n"+
			"2. Use -all flag to see comprehensive package documentation\n"+
			"3. Use -u flag to see unexported symbols if needed\n"+
			"4. For specific items, provide the target parameter (e.g., Type, Function, Method)\n\n"+
			"Example queries:\n"+
			"- Basic package docs: {\"path\": \"net/http\"}\n"+
			"- All package docs: {\"path\": \"io\", \"cmd_flags\": [\"-all\"]}\n"+
			"- Specific type: {\"path\": \"net/http\", \"target\": \"Client\"}\n"+
			"- Source code: {\"path\": \"io\", \"target\": \"Reader\", \"cmd_flags\": [\"-src\"]}\n",
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
