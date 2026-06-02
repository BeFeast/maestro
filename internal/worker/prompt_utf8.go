package worker

import (
	"os"
	"strings"
)

func sanitizePromptUTF8(prompt string) string {
	return strings.ToValidUTF8(prompt, "\uFFFD")
}

func writePromptFile(path, prompt string) error {
	return os.WriteFile(path, []byte(sanitizePromptUTF8(prompt)), 0644)
}
