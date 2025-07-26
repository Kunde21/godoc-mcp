package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jellydator/ttlcache/v3"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/sirupsen/logrus"
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
	Properties: map[string]any{
		"path": map[string]any{
			"type":        "string",
			"description": "Path to the Go package or file. This can be an import path (e.g., 'io', 'github.com/user/repo') or a local file path.",
		},
		"target": map[string]any{
			"type":        "string",
			"description": "Optional: Specific symbol to get documentation for (e.g., function name, type name, interface name). Leave empty to get full package documentation.",
		},
		"cmd_flags": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "string",
			},
			"description": "Optional: Additional go doc command flags. Common flags:\n" +
				"  -all: Show all documentation for package\n" +
				"  -src: Show the source code\n" +
				"  -u: Show unexported symbols as well as exported",
		},
		"working_dir": map[string]any{
			"type":        "string",
			"description": "Working directory to execute go doc from. Required for relative paths (including '.') to resolve the correct module context. Optional for absolute paths and standard library packages.",
		},
		"page": map[string]any{
			"type":        "integer",
			"description": "Page number (1-based) for paginated results. Default is 1.",
			"minimum":     1,
			"default":     1,
		},
		"page_size": map[string]any{
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
	cache    *ttlcache.Cache[string, cachedDoc]
	tempDirs []string // Track temporary directories for cleanup
	logger   *logrus.Logger
}

type cachedDoc struct {
	content   string
	timestamp time.Time
	byteSize  int
}

// createTempProject creates a temporary Go project with the given package
func (s *GodocServer) createTempProject(pkgPath string) (string, error) {
	switch {
	case isStdLib(pkgPath):
		// Standard library package, create a minimal temp project
	case strings.HasPrefix(pkgPath, "."),
		strings.HasPrefix(pkgPath, "/"):
		// Relative path, identify absolute path before finding go.mod
		absPath, err := filepath.Abs(pkgPath)
		if err != nil {
			break
		}
		pkgPath = absPath
		fallthrough
	case filepath.IsAbs(pkgPath):
		// Local path, try to find the module root first
		// Look for go.mod in current or parent directories
		if s1 := walkUpDir(pkgPath); s1 != "" {
			return s1, nil
		}
	}

	// Create temp project
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

	switch {
	case pkgPath == "", filepath.IsAbs(pkgPath), isStdLib(pkgPath):
		return tempDir, nil
	default:
		// Remote package, fetch the package
	}

	cmd = exec.Command("go", "get", pkgPath)
	cmd.Dir = tempDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("failed to get package %s: %v\noutput: %s", pkgPath, err, out)
	}
	return tempDir, nil
}

func walkUpDir(absPath string) string {
	stat, err := fs.Stat(os.DirFS("/"), absPath)
	if err != nil {
		return ""
	}
	if !stat.IsDir() && filepath.Ext(absPath) != ".go" {
		return ""
	}
	dir := absPath
	if !stat.IsDir() {
		dir = filepath.Dir(absPath)
	}
	for dir != "/" {
		if _, err := fs.Stat(os.DirFS(dir), "go.mod"); err == nil {
			return dir // Found valid module root
		}
		dir = filepath.Dir(dir)
	}
	return ""
}

// isStdLib checks if a package is part of the Go standard library
func isStdLib(pkg string) bool {
	// Standard library packages don't contain a dot in their import path
	return !strings.Contains(pkg, ".")
}

// cleanup removes all temporary directories and stops the cache
func (s *GodocServer) cleanup() {
	for _, dir := range s.tempDirs {
		os.RemoveAll(dir)
	}
	s.tempDirs = nil
	if s.cache != nil {
		s.cache.Stop()
	}
}

// runGoDoc executes the go doc command with the given arguments and optional working directory
func (s *GodocServer) runGoDoc(workingDir string, args ...string) (string, error) {
	// Create cache key that includes working directory
	cacheKey := workingDir + "|" + strings.Join(args, "|")

	// Check cache
	if item := s.cache.Get(cacheKey); item != nil {
		doc := item.Value()
		s.logger.WithFields(logrus.Fields{
			"cache_key": cacheKey,
			"bytes":     doc.byteSize,
		}).Debug("Cache hit")
		return doc.content, nil
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
	s.cache.Set(cacheKey, cachedDoc{
		content:   content,
		timestamp: time.Now(),
		byteSize:  len(content),
	}, 5*time.Minute)

	s.logger.WithFields(logrus.Fields{
		"cache_key": cacheKey,
		"bytes":     len(content),
	}).Debug("Cache miss")
	return content, nil
}

// validatePath ensures the path is either a valid local file/directory or appears to be a valid import path
func (*GodocServer) validatePath(path string, workingDir string) (string, error, []string) {
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
func (s *GodocServer) handleListTools() ([]any, error) {
	s.logger.Debug("handleListTools called")
	tools := []any{
		map[string]any{
			"name":        "get_doc",
			"description": toolDescription,
			"inputSchema": docInputSchema,
		},
	}
	s.logger.WithFields(logrus.Fields{
		"tools_count": len(tools),
	}).Debug("Returning tools")
	return tools, nil
}

// handleToolCall implements the tools/call endpoint
func (s *GodocServer) handleToolCall(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	s.logger.WithField("arguments", request).Debug("handleToolCall called")

	// Extract the path from arguments
	path := request.GetString("path", "")
	if path == "" {
		return &mcp.CallToolResult{
			Result: mcp.Result{
				Meta: map[string]any{},
			},
			Content: []mcp.Content{mcp.NewTextContent("invalid path parameter")},
			IsError: true,
		}, nil
	}

	// Get working directory
	workingDir := request.GetString("working_dir", "")
	if workingDir != "" {
		if info, err := os.Stat(workingDir); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("invalid working directory: %v", err)
		}
	}

	// Validate and resolve the path
	resolvedPath, err, subDirs := s.validatePath(path, workingDir)
	if err != nil {
		if subDirs == nil {
			return nil, err
		}
		// Return a special response indicating available subdirectories
		return &mcp.CallToolResult{
			Content: []mcp.Content{mcp.NewTextContent(
				fmt.Sprintf("No Go files found in %s, but found Go packages in the following subdirectories:\n%s",
					path, strings.Join(subDirs, "\n"))),
			},
			Result:  mcp.Result{Meta: map[string]any{}},
			IsError: true,
		}, nil
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

	// Add any provided command flags
	cmdArgs := request.GetStringSlice("cmd_flags", []string{})
	// Add the path
	cmdArgs = append(cmdArgs, path)

	// Add specific target if provided
	if target := request.GetString("target", ""); target != "" {
		cmdArgs = append(cmdArgs, target)
	}

	// Run go doc command with working directory
	doc, err := s.runGoDoc(workingDir, cmdArgs...)
	if err != nil {
		s.logger.WithField("error", err).Error("Error running go doc")
		return nil, err
	}

	// Get pagination parameters with defaults
	page := request.GetInt("page", 1)
	pageSize := request.GetInt("page_size", 1000)

	// Split content into lines
	lines := strings.Split(doc, "\n")
	totalLines := len(lines)
	totalPages := (totalLines + pageSize - 1) / pageSize

	// Validate page number
	if page > totalPages {
		return &mcp.CallToolResult{
			Result: mcp.Result{},
			Content: []mcp.Content{
				mcp.NewTextContent(fmt.Sprintf("page %d exceeds total pages %d", page, totalPages)),
			},
			IsError: true,
		}, nil
	}

	// Calculate slice bounds
	start := (page - 1) * pageSize
	end := min(start+pageSize, totalLines)

	// Join the lines for this page
	pageContent := strings.Join(lines[start:end], "\n")

	// Create pagination metadata
	metadata := fmt.Sprintf("Page %d of %d (showing lines %d-%d of %d)",
		page, totalPages, start+1, end, totalLines)

	// Create the result with documentation and pagination info
	result := &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.NewTextContent(metadata + "\n\n" + pageContent),
		},
		Result:  mcp.Result{},
		IsError: false,
	}

	s.logger.WithFields(logrus.Fields{
		"page":        page,
		"total_pages": totalPages,
		"lines":       end - start,
	}).Debug("Returning paginated documentation")
	return result, nil
}

func main() {
	// Set up structured logging to stderr (since stdout is used for MCP communication)
	logger := logrus.New()
	logger.SetOutput(os.Stderr)
	logger.SetLevel(logrus.InfoLevel)
	logger.Info("Starting godoc-mcp server...")

	cache := ttlcache.New(ttlcache.WithTTL[string, cachedDoc](5 * time.Minute))
	go cache.Start()

	godocServer := &GodocServer{
		cache:    cache,
		tempDirs: make([]string, 0),
		logger:   logger,
	}

	// Create new MCP server with tools enabled
	s := server.NewMCPServer(
		"godoc-mcp",
		"0.1.0",
		server.WithToolCapabilities(true), // Enable tools
		server.WithLogging(),              // Add logging
	)
	godocServer.server = s

	logger.Info("Adding get_doc tool...")
	tool := mcp.Tool{
		Name:        "get_doc",
		Description: toolDescription,
		InputSchema: docInputSchema,
		Annotations: mcp.ToolAnnotation{},
	}
	s.AddTool(tool, godocServer.handleToolCall)

	// Cleanup temporary directories before exit
	defer godocServer.cleanup()
	// Run server using stdio
	logger.Info("Starting stdio server...")
	if err := server.ServeStdio(s); err != nil {
		logger.WithFields(logrus.Fields{
			"error": err,
		}).Fatal("Server error")
	}

}
