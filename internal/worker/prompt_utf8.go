package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	workerPromptFileMode = 0o600
	workerLogDirMode     = 0o700
)

func sanitizePromptUTF8(prompt string) string {
	return strings.ToValidUTF8(prompt, "\uFFFD")
}

func writePromptFile(path, prompt string) error {
	return writeFileAtomicMode(filepath.Dir(path), path, sanitizePromptUTF8(prompt), workerPromptFileMode)
}

func ensureWorkerLogDir(path string) error {
	if err := os.MkdirAll(path, workerLogDirMode); err != nil {
		return err
	}
	dir, err := openOwnedDirNoFollow(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Chmod(workerLogDirMode); err != nil {
		return fmt.Errorf("chmod worker log dir: %w", err)
	}
	return nil
}
