package gamefleet

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// Identity/terminal evidence must have one interpretation across parsers.
// Case aliases are duplicates too because encoding/json accepts both spellings.
func uniqueObjectFields(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 64 {
			return false
		}
		token, err := d.Token()
		if err != nil {
			return false
		}
		delim, container := token.(json.Delim)
		if !container {
			return true
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				name, ok := key.(string)
				folded := strings.ToLower(name)
				// Closed DTO names are ASCII; Unicode values are permitted.
				// This also rejects non-ASCII folding aliases accepted by Go.
				if err != nil || !ok || strings.ContainsFunc(name, func(r rune) bool { return r > 127 }) || seen[folded] {
					return false
				}
				seen[folded] = true
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
		end, err := d.Token()
		return err == nil && ((delim == '{' && end == json.Delim('}')) || (delim == '[' && end == json.Delim(']')))
	}
	if !value(0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}
