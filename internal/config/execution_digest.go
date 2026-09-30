package config

import (
	"crypto/sha256"
	"encoding/hex"

	"gopkg.in/yaml.v3"
)

// AIExecutionConfigDigest pins the normalized project launch policy. Runtime
// controller references and the proof pin itself are excluded, so computing a
// reviewed manifest does not require a self-referential digest fixed point.
func AIExecutionConfigDigest(cfg *Config) (string, error) {
	copy := *cfg
	copy.AIExecution.ManifestPath = ""
	copy.AIExecution.ManifestSHA256 = ""
	b, err := yaml.Marshal(&copy)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("maestro-ai-project-config-v1\x00"), b...))
	return hex.EncodeToString(sum[:]), nil
}
