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
			"description": "Working directory to execute go doc from. Required for relative paths (including '.') to resolve the correct module context. Optional for absolute paths and standard library packages.",
		},
		"page": map[string]interface{}{
			"type":        "integer",
			"description": "Page number (1-based) for paginated results. Default is 1.",
			"minimum":     1,
			"default":     1,
		},
		"page_size": map[string]interface{}{
			"type":        "integer",
			"description": "Number of lines per page. Default is 1000. Use smaller values for very large documentation.",
			"minimum":     100,
			"maximum":     5000,
			"default":     1000,
		},
	},
	Required: []string{"path"},
}

type GodocServer struct {
	server   *server.MCPServer
	cache    map[string]cachedDoc
	tempDirs []string // Track temporary directories for cleanup
}

type cachedDoc struct {
	content   string
	timestamp time.Time
	byteSize  int
}

// createTempProject creates a temporary Go project with the given package
func (s *GodocServer) createTempProject(pkgPath string) (string, error) {
	// For standard library packages, create a minimal temp project
	if isStdLib(pkgPath) {
		tempDir, err := os.MkdirTemp("", "godoc-mcp-*")
		if err != nil {
			return "", fmt.Errorf("failed to create temp directory: %v", err)
		}
		s.tempDirs = append(s.tempDirs, tempDir)

		// Initialize minimal go.mod for std lib access
		cmd := exec.Command("go", "mod", "init", "godoc-temp")
		cmd.Dir = tempDir
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("failed to initialize go.mod: %v\noutput: %s", err, out)
		}
		return tempDir, nil
	}

	// For local paths, try to find the module root first
	if strings.HasPrefix(pkgPath, ".") || strings.HasPrefix(pkgPath, "/") || filepath.IsAbs(pkgPath) {
		absPath, err := filepath.Abs(pkgPath)
		if err == nil {
			// Look for go.mod in current or parent directories
			dir := absPath
			for dir != "/" {
				if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
					// Verify this module contains our target package
					if info, err := os.Stat(absPath); err == nil {
						if info.IsDir() || strings.HasSuffix(absPath, ".go") {
							return dir, nil // Found valid module root
						}
					}
				}
				dir = filepath.Dir(dir)
			}
		}
	}

	// For non-local paths or if no module root found, create temp project
	tempDir, err := os.MkdirTemp("", "godoc-mcp-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp directory: %v", err)
	}
	s.tempDirs = append(s.tempDirs, tempDir)

	// Initialize go.mod
	cmd := exec.Command("go", "mod", "init", "godoc-temp")
	cmd.Dir = tempDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("failed to initialize go.mod: %v\noutput: %s", err, out)
	}

	// For remote packages, try to get the package
	if pkgPath != "" && !strings.HasPrefix(pkgPath, ".") && !strings.HasPrefix(pkgPath, "/") && !filepath.IsAbs(pkgPath) {
		cmd = exec.Command("go", "get", pkgPath)
		cmd.Dir = tempDir
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("failed to get package %s: %v\noutput: %s", pkgPath, err, out)
		}
	}

	return tempDir, nil
}

// isStdLib checks if a package is part of the Go standard library
func isStdLib(pkg string) bool {
	// Standard library packages don't contain a dot in their import path
	return !strings.Contains(pkg, ".")
}

// cleanup removes all temporary directories
func (s *GodocServer) cleanup() {
	for _, dir := range s.tempDirs {
		os.RemoveAll(dir)
	}
	s.tempDirs = nil
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
func (s *GodocServer) validatePath(path string, workingDir string) (string, error, []string) {
	// For relative paths, working directory is required
	if strings.HasPrefix(path, ".") {
		if workingDir == "" {
			return "", fmt.Errorf("working_dir is required for relative paths (including '.')"), nil
		}

		// Read go.mod from working directory only
		modPath := filepath.Join(workingDir, "go.mod")
		content, err := os.ReadFile(modPath)
		if err != nil {
			return "", fmt.Errorf("failed to read go.mod in working directory: %v", err), nil
		}

		// Parse module name from go.mod
		var moduleName string
		for _, line := range strings.Split(string(content), "\n") {
			if strings.HasPrefix(line, "module ") {
				moduleName = strings.TrimSpace(strings.TrimPrefix(line, "module "))
				break
			}
		}
		if moduleName == "" {
			return "", fmt.Errorf("no module declaration found in go.mod"), nil
		}

		// If path is ".", use the module name directly
		if path == "." {
			return moduleName, nil, nil
		}

		// For other relative paths, append to module name
		relPath := strings.TrimPrefix(path, "./")
		return filepath.Join(moduleName, relPath), nil, nil
	}

	// Handle absolute paths - must match working directory if provided
	if strings.HasPrefix(path, "/") || filepath.IsAbs(path) {
		if workingDir != "" && path != workingDir {
			return "", fmt.Errorf("absolute path must match working directory when provided"), nil
		}

		// Read go.mod from the absolute path
		modPath := filepath.Join(path, "go.mod")
		content, err := os.ReadFile(modPath)
		if err != nil {
			return "", fmt.Errorf("failed to read go.mod: %v", err), nil
		}

		// Parse module name from go.mod
		var moduleName string
		for _, line := range strings.Split(string(content), "\n") {
			if strings.HasPrefix(line, "module ") {
				moduleName = strings.TrimSpace(strings.TrimPrefix(line, "module "))
				return moduleName, nil, nil
			}
		}
		return "", fmt.Errorf("no module declaration found in go.mod"), nil
	}

	// For all other paths, treat as import path
	return path, nil, nil
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

	// Get working directory
	workingDir := ""
	if wd, ok := arguments["working_dir"].(string); ok && wd != "" {
		if info, err := os.Stat(wd); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("invalid working directory: %v", err)
		}
		workingDir = wd
	}

	// Validate and resolve the path
	resolvedPath, err, subDirs := s.validatePath(path, workingDir)
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

	// Use the resolved path for documentation
	path = resolvedPath

	// Create temporary project if needed
	if workingDir == "" {
		var err error
		workingDir, err = s.createTempProject(path)
		if err != nil {
			return nil, fmt.Errorf("failed to create temporary project: %v", err)
		}
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

	// Get pagination parameters with defaults
	page := 1
	if p, ok := arguments["page"].(float64); ok {
		page = int(p)
	}
	pageSize := 1000
	if ps, ok := arguments["page_size"].(float64); ok {
		pageSize = int(ps)
	}

	// Split content into lines
	lines := strings.Split(doc, "\n")
	totalLines := len(lines)
	totalPages := (totalLines + pageSize - 1) / pageSize

	// Validate page number
	if page > totalPages {
		return nil, fmt.Errorf("page %d exceeds total pages %d", page, totalPages)
	}

	// Calculate slice bounds
	start := (page - 1) * pageSize
	end := start + pageSize
	if end > totalLines {
		end = totalLines
	}

	// Join the lines for this page
	pageContent := strings.Join(lines[start:end], "\n")

	// Create pagination metadata
	metadata := fmt.Sprintf("Page %d of %d (showing lines %d-%d of %d)",
		page, totalPages, start+1, end, totalLines)

	// Create the result with documentation and pagination info
	result := &mcp.CallToolResult{
		Content: []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": metadata + "\n\n" + pageContent,
			},
		},
	}

	log.Printf("Returning page %d/%d (%d lines)", page, totalPages, end-start)
	return result, nil
}

func main() {
	// Set up logging to stderr (since stdout is used for MCP communication)
	log.SetOutput(os.Stderr)
	log.Printf("Starting godoc-mcp server...")

	godocServer := &GodocServer{
		cache:    make(map[string]cachedDoc),
		tempDirs: make([]string, 0),
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

	// Cleanup temporary directories before exit
	godocServer.cleanup()
}
