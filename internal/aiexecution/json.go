package aiexecution

import (
	"bytes"
	"encoding/json"
	"io"
)

// DecodeStrict refuses ambiguous duplicate members before typed decoding.
// Control-plane evidence cannot choose a first-key/last-key interpretation.
func DecodeStrict(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 32 {
			return false
		}
		token, err := d.Token()
		if err != nil {
			return false
		}
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					key, err := d.Token()
					name, ok := key.(string)
					if err != nil || !ok || seen[name] {
						return false
					}
					seen[name] = true
					if !value(depth + 1) {
						return false
					}
				}
			case '[':
				for d.More() {
					if !value(depth + 1) {
						return false
					}
				}
			default:
				return false
			}
			_, err = d.Token()
			return err == nil
		}
		return true
	}
	if !value(0) {
		return Held("evidence_json_invalid")
	}
	if _, err := d.Token(); err != io.EOF {
		return Held("evidence_json_invalid")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(target) != nil || d.Decode(new(any)) != io.EOF {
		return Held("evidence_json_invalid")
	}
	return nil
}
