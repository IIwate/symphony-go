package loader

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"symphony-go/internal/model"
)

const watchDebounce = 250 * time.Millisecond

func Watch(ctx context.Context, dir string, profile string, onChange func(*model.AutomationDefinition)) error {
	return WatchWithErrors(ctx, dir, profile, onChange, nil)
}

func WatchWithErrors(ctx context.Context, dir string, profile string, onChange func(*model.AutomationDefinition), onError func(error)) error {
	initialDef, err := Load(dir, profile)
	if err != nil {
		return err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	for _, watchDir := range watchDirectories(initialDef.RootDir) {
		info, statErr := os.Stat(watchDir)
		if statErr != nil || !info.IsDir() {
			continue
		}
		if err := watcher.Add(watchDir); err != nil {
			watcher.Close()
			return err
		}
	}

	go func() {
		defer watcher.Close()

		var timer *time.Timer
		var timerC <-chan time.Time
		watchedPaths := buildWatchedPaths(initialDef)

		resetTimer := func() {
			if timer == nil {
				timer = time.NewTimer(watchDebounce)
				timerC = timer.C
				return
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(watchDebounce)
			timerC = timer.C
		}

		for {
			select {
			case <-ctx.Done():
				if timer != nil {
					timer.Stop()
				}
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
					continue
				}
				if !matchesWatchedPathSet(watchedPaths, event.Name) {
					continue
				}
				resetTimer()
			case <-timerC:
				timerC = nil
				definition, loadErr := Load(initialDef.RootDir, profile)
				if loadErr != nil {
					if onError != nil {
						onError(loadErr)
					}
					continue
				}
				if onChange != nil {
					onChange(definition)
				}
			case watchErr, ok := <-watcher.Errors:
				if !ok {
					return
				}
				if onError != nil {
					onError(watchErr)
				}
			}
		}
	}()

	return nil
}

func watchDirectories(rootDir string) []string {
	return []string{
		rootDir,
		filepath.Join(rootDir, "profiles"),
		filepath.Join(rootDir, "sources"),
		filepath.Join(rootDir, "flows"),
		filepath.Join(rootDir, "prompts"),
		filepath.Join(rootDir, "policies"),
		filepath.Join(rootDir, "hooks"),
		filepath.Join(rootDir, "local"),
	}
}

func buildWatchedPaths(def *model.AutomationDefinition) map[string]struct{} {
	paths := map[string]struct{}{
		cleanComparablePath(filepath.Join(def.RootDir, "project.yaml")):            {},
		cleanComparablePath(filepath.Join(def.RootDir, "local", "overrides.yaml")): {},
	}
	if strings.TrimSpace(def.Profile) != "" {
		paths[cleanComparablePath(filepath.Join(def.RootDir, "profiles", def.Profile+".yaml"))] = struct{}{}
	}

	for name := range def.Sources {
		paths[cleanComparablePath(filepath.Join(def.RootDir, "sources", name+".yaml"))] = struct{}{}
	}
	for name := range def.Flows {
		paths[cleanComparablePath(filepath.Join(def.RootDir, "flows", name+".yaml"))] = struct{}{}
	}
	for name := range def.Policies {
		paths[cleanComparablePath(filepath.Join(def.RootDir, "policies", name+".yaml"))] = struct{}{}
	}

	promptDir := filepath.Join(def.RootDir, "prompts")
	hookDir := filepath.Join(def.RootDir, "hooks")
	if entries, err := os.ReadDir(promptDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md.liquid") {
				continue
			}
			paths[cleanComparablePath(filepath.Join(promptDir, entry.Name()))] = struct{}{}
		}
	}
	if entries, err := os.ReadDir(hookDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".sh" {
				continue
			}
			paths[cleanComparablePath(filepath.Join(hookDir, entry.Name()))] = struct{}{}
		}
	}

	return paths
}

func matchesWatchedPathSet(paths map[string]struct{}, eventPath string) bool {
	comparable := cleanComparablePath(eventPath)
	_, ok := paths[comparable]
	return ok
}

func cleanComparablePath(path string) string {
	absPath, err := filepath.Abs(path)
	if err != nil {
		absPath = filepath.Clean(path)
	}
	if runtime.GOOS == "windows" {
		return strings.ToLower(absPath)
	}
	return absPath
}
