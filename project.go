package main

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// TODO: Extract project directory management.  Cache project directories, not output.
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
