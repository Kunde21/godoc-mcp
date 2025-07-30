package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jellydator/ttlcache/v3"
	"github.com/sirupsen/logrus"
)

// ProjectManager manages temporary Go project directories with caching
type ProjectManager struct {
	cache    *ttlcache.Cache[string, string]
	tempDirs []string
	mu       sync.Mutex
	logger   *logrus.Logger
}

// NewProjectManager creates a new ProjectManager with caching
func NewProjectManager(logger *logrus.Logger) *ProjectManager {
	pm := &ProjectManager{
		cache:    ttlcache.New(ttlcache.WithTTL[string, string](30 * time.Minute)),
		tempDirs: make([]string, 0),
		logger:   logger,
	}
	pm.cache.OnEviction(func(ctx context.Context, er ttlcache.EvictionReason, i *ttlcache.Item[string, string]) {
		pm.mu.Lock()
		defer pm.mu.Unlock()
		os.RemoveAll(i.Value())
		pm.tempDirs = slices.DeleteFunc(pm.tempDirs, func(s string) bool { return s == i.Value() })
	})
	go pm.cache.Start()
	return pm
}

// GetOrCreateProject gets or creates a temporary Go project for the given package path
func (pm *ProjectManager) GetOrCreateProject(pkgPath string) (string, error) {
	// Check cache first
	if item := pm.cache.Get(pkgPath); item != nil {
		projectDir := item.Value()
		pm.logger.WithField("package", pkgPath).WithField("project_dir", projectDir).Debug("Project cache hit")
		return projectDir, nil
	}

	pm.logger.WithField("package", pkgPath).Debug("Project cache miss, creating new project")

	// Create new project
	projectDir, err := pm.createTempProject(pkgPath)
	if err != nil {
		return "", err
	}

	// Cache the project directory
	pm.cache.Set(pkgPath, projectDir, 30*time.Minute)
	pm.logger.WithField("package", pkgPath).WithField("project_dir", projectDir).Debug("Project cached")

	return projectDir, nil
}

// createTempProject creates a temporary Go project with the given package
func (pm *ProjectManager) createTempProject(pkgPath string) (string, error) {
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

	pm.mu.Lock()
	pm.tempDirs = append(pm.tempDirs, tempDir)
	pm.mu.Unlock()

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

// cleanup removes all temporary directories and stops the cache
func (pm *ProjectManager) cleanup() {
	if pm == nil {
		return
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.tempDirs = nil
	pm.cache.DeleteAll()
	pm.cache.Stop()
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
