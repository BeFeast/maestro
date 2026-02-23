package watch

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/state"
)

const WatchSession = "maestro-watch"

// PaneMapFile is the default path for the pane mapping file.
// Only one watch session exists at a time, so a fixed path is safe.
const PaneMapFile = "/tmp/maestro-watch-panes.json"

// PaneMapping maps a tmux pane index to a worker slot and its state directory.
type PaneMapping struct {
	PaneIndex int    `json:"pane_index"`
	SlotName  string `json:"slot_name"`
	StateDir  string `json:"state_dir"`
}

// WritePaneMap writes pane mappings to a JSON file.
func WritePaneMap(path string, mappings []PaneMapping) error {
	data, err := json.Marshal(mappings)
	if err != nil {
		return fmt.Errorf("marshal pane map: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

// ReadPaneMap reads pane mappings from a JSON file.
func ReadPaneMap(path string) ([]PaneMapping, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read pane map: %w", err)
	}
	var mappings []PaneMapping
	if err := json.Unmarshal(data, &mappings); err != nil {
		return nil, fmt.Errorf("parse pane map: %w", err)
	}
	return mappings, nil
}

// RunUpdater periodically updates pane titles in the watch session.
// It exits when the watch session no longer exists.
func RunUpdater(mapFile string, interval time.Duration) {
	mappings, err := ReadPaneMap(mapFile)
	if err != nil {
		log.Fatalf("[watch-updater] %v", err)
	}

	log.Printf("[watch-updater] started with %d panes, interval=%s", len(mappings), interval)

	for {
		if !sessionExists(WatchSession) {
			log.Printf("[watch-updater] watch session gone, exiting")
			os.Remove(mapFile)
			return
		}

		// Load state files (deduplicate by state dir)
		stateCache := make(map[string]map[string]*state.Session)
		for _, m := range mappings {
			if _, ok := stateCache[m.StateDir]; ok {
				continue
			}
			s, err := state.Load(m.StateDir)
			if err != nil {
				log.Printf("[watch-updater] load state %s: %v", m.StateDir, err)
				continue
			}
			stateCache[m.StateDir] = s.Sessions
		}

		for _, m := range mappings {
			sessions, ok := stateCache[m.StateDir]
			if !ok {
				continue
			}
			sess, ok := sessions[m.SlotName]
			if !ok {
				continue
			}

			title := FormatPaneTitle(m.SlotName, sess)

			// Best-effort: capture last meaningful output line from the pane
			if lastLine := captureLastLine(m.PaneIndex); lastLine != "" {
				if len(lastLine) > 60 {
					lastLine = lastLine[:57] + "..."
				}
				title += "│ " + lastLine + " "
			}

			paneTarget := fmt.Sprintf("%s:0.%d", WatchSession, m.PaneIndex)
			exec.Command("tmux", "select-pane", "-t", paneTarget, "-T", title).Run()
		}

		time.Sleep(interval)
	}
}

// sessionExists checks whether a tmux session exists.
func sessionExists(name string) bool {
	return exec.Command("tmux", "has-session", "-t", name).Run() == nil
}

// captureLastLine gets the last non-empty, non-spinner line from a watch pane.
func captureLastLine(paneIndex int) string {
	paneTarget := fmt.Sprintf("%s:0.%d", WatchSession, paneIndex)
	out, err := exec.Command("tmux", "capture-pane", "-t", paneTarget, "-p", "-l", "5").Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(string(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if cleaned := CleanOutputLine(lines[i]); cleaned != "" {
			return cleaned
		}
	}
	return ""
}
