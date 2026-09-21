package console

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// SetPassword replaces only the private configuration. The running console must
// then be restarted so its cached password and all in-memory sessions expire.
func SetPassword(path, password string) error {
	before, info, err := readPrivateFile(path, configSizeLimit, -1)
	if err != nil {
		return errors.New("cannot read private configuration")
	}
	var cfg Config
	decoder := json.NewDecoder(bytes.NewReader(before))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cfg) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid configuration JSON")
	}
	if err = cfg.validate(); err != nil {
		return err
	}
	encoded, err := HashPassword(password)
	if err != nil {
		return err
	}
	// Retain other values exactly at the JSON value level, including omitted
	// defaults; updating a password must not silently rewrite deployment settings.
	var fields map[string]json.RawMessage
	if json.Unmarshal(before, &fields) != nil {
		return errors.New("invalid configuration JSON")
	}
	fields["password_hash"], _ = json.Marshal(encoded)
	after, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return errors.New("cannot encode private configuration")
	}
	return replacePrivateConfig(path, before, info, append(after, '\n'))
}
