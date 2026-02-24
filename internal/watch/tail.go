package watch

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// TailFiltered tails a log file, applying CleanOutputLine to each line.
// It exits when the specified tmux session no longer exists (worker finished).
func TailFiltered(logFile string, tmuxSession string) {
	// Wait for log file to appear (only while session is alive)
	for {
		if _, err := os.Stat(logFile); err == nil {
			break
		}
		if !sessionExists(tmuxSession) {
			fmt.Println("No log file found.")
			return
		}
		time.Sleep(500 * time.Millisecond)
	}

	f, err := os.Open(logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open log: %v\n", err)
		return
	}
	defer f.Close()

	var partial string
	buf := make([]byte, 4096)

	for {
		n, err := f.Read(buf)
		if n > 0 {
			data := partial + string(buf[:n])
			partial = ""

			lines := strings.Split(data, "\n")
			// Last element is a partial line if data doesn't end with newline
			if !strings.HasSuffix(data, "\n") {
				partial = lines[len(lines)-1]
				lines = lines[:len(lines)-1]
			}

			for _, line := range lines {
				if cleaned := CleanOutputLine(line); cleaned != "" {
					fmt.Println(cleaned)
				}
			}
		}
		if err == io.EOF {
			if !sessionExists(tmuxSession) {
				// Drain any remaining partial line
				if partial != "" {
					if cleaned := CleanOutputLine(partial); cleaned != "" {
						fmt.Println(cleaned)
					}
				}
				return
			}
			time.Sleep(300 * time.Millisecond)
			continue
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "read log: %v\n", err)
			return
		}
	}
}
