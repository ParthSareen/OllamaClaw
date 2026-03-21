package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/parth/ollamaclaw/internal/config"
	"github.com/parth/ollamaclaw/internal/db"
	"github.com/parth/ollamaclaw/internal/tools"
	"github.com/parth/ollamaclaw/internal/util"
)

const manifestName = "claw.plugin.json"

type Manager struct {
	store             *db.Store
	cfg               config.Config
	pluginCallTimeout int
}

func NewManager(store *db.Store, cfg config.Config) *Manager {
	return &Manager{store: store, cfg: cfg, pluginCallTimeout: cfg.PluginCallTimeoutSec}
}

func (m *Manager) LoadEnabledTools(ctx context.Context) ([]tools.Tool, error) {
	ptools, err := m.store.ListEnabledPluginTools(ctx)
	if err != nil {
		return nil, err
	}
	lock, err := m.loadLockFile()
	if err != nil {
		return nil, err
	}
	byPlugin := map[string]LockEntry{}
	for _, p := range lock.Plugins {
		byPlugin[p.ID] = p
	}

	var out []tools.Tool
	for _, pt := range ptools {
		pluginRow, ok, err := m.store.GetPlugin(ctx, pt.PluginID)
		if err != nil || !ok {
			continue
		}
		entry, ok := byPlugin[pt.PluginID]
		if !ok {
			continue
		}
		checksum, err := util.HashDirectory(pluginRow.InstallPath)
		if err != nil || checksum != entry.Checksum {
			continue
		}
		manifestPath := filepath.Join(pluginRow.InstallPath, manifestName)
		manifest, err := parseManifest(manifestPath)
		if err != nil {
			continue
		}
		var schema json.RawMessage
		if strings.TrimSpace(pt.SchemaJSON) != "" {
			schema = json.RawMessage(pt.SchemaJSON)
		}
		pluginID := pt.PluginID
		toolName := pt.ToolName
		timeout := pt.TimeoutSec
		if timeout <= 0 {
			timeout = m.pluginCallTimeout
		}
		installPath := pluginRow.InstallPath
		manifestCopy := manifest
		out = append(out, tools.Tool{
			Name:        toolName,
			Description: pt.Description,
			Schema:      schema,
			Source:      "plugin",
			PluginID:    pluginID,
			TimeoutSec:  timeout,
			Execute: func(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
				return CallTool(ctx, installPath, manifestCopy, timeout, toolName, args)
			},
		})
	}
	return out, nil
}

func (m *Manager) Install(ctx context.Context, source string) (db.Plugin, []db.PluginTool, error) {
	stagingDir, resolvedRef, err := m.fetchToStaging(ctx, source)
	if err != nil {
		return db.Plugin{}, nil, err
	}
	defer os.RemoveAll(stagingDir)

	pluginDir, err := findPluginRoot(stagingDir)
	if err != nil {
		return db.Plugin{}, nil, err
	}
	manifest, err := parseManifest(filepath.Join(pluginDir, manifestName))
	if err != nil {
		return db.Plugin{}, nil, err
	}
	if manifest.APIVersion == "" {
		manifest.APIVersion = "1.0"
	}
	installDir, err := m.installPath(manifest)
	if err != nil {
		return db.Plugin{}, nil, err
	}
	if err := os.RemoveAll(installDir); err != nil {
		return db.Plugin{}, nil, fmt.Errorf("clear old install: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(installDir), 0o755); err != nil {
		return db.Plugin{}, nil, err
	}
	if err := util.CopyDir(pluginDir, installDir); err != nil {
		return db.Plugin{}, nil, err
	}
	checksum, err := util.HashDirectory(installDir)
	if err != nil {
		return db.Plugin{}, nil, err
	}
	permissionsJSON, _ := json.Marshal(manifest.Permissions)

	prev, ok, _ := m.store.GetPlugin(ctx, manifest.ID)
	if ok && strings.TrimSpace(prev.Permissions) != strings.TrimSpace(string(permissionsJSON)) {
		fmt.Fprintf(os.Stderr, "warning: plugin %s permissions changed\n", manifest.ID)
	}

	dbPlugin := db.Plugin{
		ID:          manifest.ID,
		Name:        manifest.Name,
		Version:     manifest.Version,
		Source:      source,
		ResolvedRef: resolvedRef,
		Checksum:    checksum,
		InstallPath: installDir,
		Permissions: string(permissionsJSON),
		Enabled:     true,
	}
	if err := m.store.UpsertPlugin(ctx, dbPlugin); err != nil {
		return db.Plugin{}, nil, err
	}

	descriptors, err := ProbeTools(ctx, installDir, manifest, m.pluginCallTimeout)
	if err != nil {
		return db.Plugin{}, nil, fmt.Errorf("probe plugin tools: %w", err)
	}
	pluginTools := make([]db.PluginTool, 0, len(descriptors))
	for _, d := range descriptors {
		schema := "{}"
		if len(d.Parameters) > 0 {
			schema = string(d.Parameters)
		}
		timeout := d.TimeoutSec
		if timeout <= 0 {
			timeout = m.pluginCallTimeout
		}
		pluginTools = append(pluginTools, db.PluginTool{
			PluginID:    manifest.ID,
			ToolName:    d.Name,
			Description: d.Description,
			SchemaJSON:  schema,
			TimeoutSec:  timeout,
			Enabled:     true,
		})
	}
	if err := m.store.ReplacePluginTools(ctx, manifest.ID, pluginTools); err != nil {
		return db.Plugin{}, nil, err
	}
	if err := m.updateLock(dbPlugin); err != nil {
		return db.Plugin{}, nil, err
	}
	return dbPlugin, pluginTools, nil
}

func (m *Manager) Update(ctx context.Context, pluginID string) error {
	if strings.TrimSpace(pluginID) != "" {
		p, ok, err := m.store.GetPlugin(ctx, pluginID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("plugin %s not found", pluginID)
		}
		_, _, err = m.Install(ctx, p.Source)
		return err
	}
	plugins, err := m.store.ListPlugins(ctx, false)
	if err != nil {
		return err
	}
	for _, p := range plugins {
		if _, _, err := m.Install(ctx, p.Source); err != nil {
			return fmt.Errorf("update %s: %w", p.ID, err)
		}
	}
	return nil
}

func (m *Manager) Remove(ctx context.Context, pluginID string) error {
	p, ok, err := m.store.GetPlugin(ctx, pluginID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("plugin %s not found", pluginID)
	}
	if err := m.store.DeletePlugin(ctx, pluginID); err != nil {
		return err
	}
	_ = os.RemoveAll(p.InstallPath)
	if err := m.removeLockEntry(pluginID); err != nil {
		return err
	}
	return nil
}

func (m *Manager) SetEnabled(ctx context.Context, pluginID string, enabled bool) error {
	return m.store.SetPluginEnabled(ctx, pluginID, enabled)
}

func (m *Manager) fetchToStaging(ctx context.Context, source string) (string, string, error) {
	tempRoot, err := os.MkdirTemp("", "ollamaclaw-plugin-*")
	if err != nil {
		return "", "", err
	}
	staging := filepath.Join(tempRoot, "src")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", "", err
	}

	source = strings.TrimSpace(source)
	if source == "" {
		return "", "", fmt.Errorf("plugin source cannot be empty")
	}

	if looksLikeLocalPath(source) {
		source = expandHome(source)
		abs, err := filepath.Abs(source)
		if err != nil {
			return "", "", err
		}
		if err := util.CopyDir(abs, staging); err != nil {
			return "", "", err
		}
		return tempRoot, abs, nil
	}

	if isArchiveURL(source) {
		b, err := download(ctx, source)
		if err != nil {
			return "", "", err
		}
		if strings.HasSuffix(strings.ToLower(source), ".zip") {
			if err := util.ExtractZip(b, staging); err != nil {
				return "", "", err
			}
		} else {
			if err := util.ExtractTarGz(b, staging); err != nil {
				return "", "", err
			}
		}
		return tempRoot, source, nil
	}

	gitSource, ref := parseGitSource(source)
	args := []string{"clone", "--depth", "1"}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	args = append(args, gitSource, staging)
	cmd := exec.CommandContext(ctx, "git", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("git clone failed: %s", strings.TrimSpace(out.String()))
	}
	shaCmd := exec.CommandContext(ctx, "git", "-C", staging, "rev-parse", "HEAD")
	shaBytes, err := shaCmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("git rev-parse failed: %w", err)
	}
	resolved := strings.TrimSpace(string(shaBytes))
	if resolved == "" {
		resolved = gitSource
	}
	return tempRoot, resolved, nil
}

func (m *Manager) installPath(manifest Manifest) (string, error) {
	base, err := config.PluginsDir()
	if err != nil {
		return "", err
	}
	idSafe := strings.ReplaceAll(manifest.ID, "/", "_")
	return filepath.Join(base, idSafe, manifest.Version), nil
}

func (m *Manager) loadLockFile() (LockFile, error) {
	path, err := config.PluginsLockPath()
	if err != nil {
		return LockFile{}, err
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return LockFile{Version: 1, Plugins: []LockEntry{}}, nil
	}
	if err != nil {
		return LockFile{}, err
	}
	var lock LockFile
	if err := json.Unmarshal(b, &lock); err != nil {
		return LockFile{}, err
	}
	if lock.Version == 0 {
		lock.Version = 1
	}
	return lock, nil
}

func (m *Manager) saveLockFile(lock LockFile) error {
	path, err := config.PluginsLockPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func (m *Manager) updateLock(p db.Plugin) error {
	lock, err := m.loadLockFile()
	if err != nil {
		return err
	}
	entry := LockEntry{
		ID:          p.ID,
		Version:     p.Version,
		Source:      p.Source,
		ResolvedRef: p.ResolvedRef,
		Checksum:    p.Checksum,
		InstalledAt: time.Now().UTC().Format(time.RFC3339Nano),
		Path:        p.InstallPath,
	}
	found := false
	for i := range lock.Plugins {
		if lock.Plugins[i].ID == p.ID {
			lock.Plugins[i] = entry
			found = true
			break
		}
	}
	if !found {
		lock.Plugins = append(lock.Plugins, entry)
	}
	sort.Slice(lock.Plugins, func(i, j int) bool { return lock.Plugins[i].ID < lock.Plugins[j].ID })
	return m.saveLockFile(lock)
}

func (m *Manager) removeLockEntry(pluginID string) error {
	lock, err := m.loadLockFile()
	if err != nil {
		return err
	}
	out := lock.Plugins[:0]
	for _, p := range lock.Plugins {
		if p.ID != pluginID {
			out = append(out, p)
		}
	}
	lock.Plugins = out
	return m.saveLockFile(lock)
}

func findPluginRoot(staging string) (string, error) {
	candidate := filepath.Join(staging, manifestName)
	if _, err := os.Stat(candidate); err == nil {
		return staging, nil
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		return "", err
	}
	if len(entries) == 1 && entries[0].IsDir() {
		sub := filepath.Join(staging, entries[0].Name())
		if _, err := os.Stat(filepath.Join(sub, manifestName)); err == nil {
			return sub, nil
		}
	}
	return "", fmt.Errorf("%s not found in source", manifestName)
}

func parseGitSource(source string) (string, string) {
	s := strings.TrimPrefix(source, "git:")
	if strings.Contains(s, "@") {
		idx := strings.LastIndex(s, "@")
		if idx > strings.LastIndex(s, "/") {
			return s[:idx], s[idx+1:]
		}
	}
	return s, ""
}

func isArchiveURL(source string) bool {
	s := strings.ToLower(source)
	isHTTP := strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
	return isHTTP && (strings.HasSuffix(s, ".zip") || strings.HasSuffix(s, ".tar.gz") || strings.HasSuffix(s, ".tgz"))
}

func looksLikeLocalPath(source string) bool {
	if strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../") || strings.HasPrefix(source, "/") || strings.HasPrefix(source, "~/") {
		return true
	}
	_, err := os.Stat(source)
	return err == nil
}

func expandHome(source string) string {
	if strings.HasPrefix(source, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(source, "~/"))
		}
	}
	return source
}

func download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return nil, fmt.Errorf("download status %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	return io.ReadAll(res.Body)
}
