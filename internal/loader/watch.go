package loader

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"symphony-go/internal/model"
)

const watchDebounce = 250 * time.Millisecond

func Watch(ctx context.Context, dir string, profile string, onChange func(*model.AutomationDefinition)) error {
	return WatchWithErrors(ctx, dir, profile, func(def *model.AutomationDefinition) error {
		if onChange != nil {
			onChange(def)
		}
		return nil
	}, nil)
}

func WatchWithErrors(ctx context.Context, dir string, profile string, onChange func(*model.AutomationDefinition) error, onError func(error)) error {
	initialDef, err := Load(dir, profile)
	if err != nil {
		return err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	watchedDirs := map[string]struct{}{}
	if err := addWatchDirectories(watcher, watchedDirs, initialDef.RootDir); err != nil {
		watcher.Close()
		return err
	}

	go func() {
		defer watcher.Close()

		var timer *time.Timer
		var timerC <-chan time.Time
		explicitProfile := strings.TrimSpace(profile)
		acceptedProfile := strings.TrimSpace(initialDef.Profile)

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
				if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
					delete(watchedDirs, cleanComparablePath(event.Name))
				}
				if event.Op&fsnotify.Create != 0 {
					if err := addWatchDirectory(watcher, watchedDirs, initialDef.RootDir, event.Name); err != nil {
						if onError != nil {
							onError(err)
						}
					}
				}
				if !matchesWatchedPath(initialDef.RootDir, acceptedProfile, event.Name) {
					continue
				}
				resetTimer()
			case <-timerC:
				timerC = nil
				definition, loadErr := Load(initialDef.RootDir, acceptedProfile)
				if loadErr != nil {
					if onError != nil {
						onError(loadErr)
					}
					continue
				}
				if explicitProfile == "" {
					selectedDefinition, selectedErr := Load(initialDef.RootDir, "")
					switch {
					case selectedErr == nil && strings.TrimSpace(selectedDefinition.Profile) != acceptedProfile:
						if onChange == nil {
							definition = selectedDefinition
							acceptedProfile = strings.TrimSpace(selectedDefinition.Profile)
						} else if err := onChange(selectedDefinition); err != nil {
							if onError != nil {
								onError(err)
							}
						} else {
							definition = selectedDefinition
							acceptedProfile = strings.TrimSpace(selectedDefinition.Profile)
							if err := addWatchDirectories(watcher, watchedDirs, definition.RootDir); err != nil && onError != nil {
								onError(err)
							}
							continue
						}
					case selectedErr != nil && onError != nil:
						onError(selectedErr)
					}
				}
				if err := addWatchDirectories(watcher, watchedDirs, definition.RootDir); err != nil {
					if onError != nil {
						onError(err)
					}
				}
				if onChange != nil {
					if err := onChange(definition); err != nil {
						if onError != nil {
							onError(err)
						}
						continue
					}
				}
				acceptedProfile = strings.TrimSpace(definition.Profile)
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

func addWatchDirectories(watcher *fsnotify.Watcher, watchedDirs map[string]struct{}, rootDir string) error {
	for _, watchDir := range watchDirectories(rootDir) {
		if err := addWatchDirectory(watcher, watchedDirs, rootDir, watchDir); err != nil {
			return err
		}
	}
	return nil
}

func addWatchDirectory(watcher *fsnotify.Watcher, watchedDirs map[string]struct{}, rootDir string, dir string) error {
	if !isWatchDirectory(rootDir, dir) {
		return nil
	}

	comparable := cleanComparablePath(dir)
	if _, exists := watchedDirs[comparable]; exists {
		return nil
	}

	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}
	if err := watcher.Add(dir); err != nil {
		return err
	}
	watchedDirs[comparable] = struct{}{}
	return nil
}

func matchesWatchedPath(rootDir string, activeProfile string, eventPath string) bool {
	relativePath, ok := relativeWatchPath(rootDir, eventPath)
	if !ok {
		return false
	}

	switch relativePath {
	case "project.yaml", "local/overrides.yaml":
		return true
	case "local/env.local":
		return false
	}
	if strings.HasPrefix(relativePath, "local/") {
		return false
	}

	switch {
	case strings.HasPrefix(relativePath, "profiles/"):
		if activeProfile == "" {
			return matchesDirectoryFile(relativePath, "profiles", ".yaml")
		}
		return relativePath == path.Join("profiles", activeProfile+".yaml")
	case strings.HasPrefix(relativePath, "sources/"):
		return matchesDirectoryFile(relativePath, "sources", ".yaml")
	case strings.HasPrefix(relativePath, "flows/"):
		return matchesDirectoryFile(relativePath, "flows", ".yaml")
	case strings.HasPrefix(relativePath, "prompts/"):
		return matchesDirectoryFile(relativePath, "prompts", ".md.liquid")
	case strings.HasPrefix(relativePath, "policies/"):
		return matchesDirectoryFile(relativePath, "policies", ".yaml")
	case strings.HasPrefix(relativePath, "hooks/"):
		return matchesDirectoryFile(relativePath, "hooks", ".py")
	default:
		return false
	}
}

func isWatchDirectory(rootDir string, eventPath string) bool {
	relativePath, ok := relativeWatchPath(rootDir, eventPath)
	if !ok {
		return false
	}

	switch relativePath {
	case ".", "profiles", "sources", "flows", "prompts", "policies", "hooks", "local":
		return true
	default:
		return false
	}
}

func relativeWatchPath(rootDir string, eventPath string) (string, bool) {
	absEventPath, err := filepath.Abs(eventPath)
	if err != nil {
		return "", false
	}
	relativePath, err := filepath.Rel(rootDir, absEventPath)
	if err != nil {
		return "", false
	}
	if relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return filepath.ToSlash(filepath.Clean(relativePath)), true
}

func matchesDirectoryFile(relativePath string, directory string, suffix string) bool {
	return path.Dir(relativePath) == directory && strings.HasSuffix(path.Base(relativePath), suffix)
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
