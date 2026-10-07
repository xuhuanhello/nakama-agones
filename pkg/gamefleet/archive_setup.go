package gamefleet

import (
	"errors"
	"io"
	"os"
	"strings"
)

// ArchiveConfigFromEnv is disabled when all archive settings are empty.
// A partial configuration fails startup; it never inherits the active key,
// application, issuer, region or compatibility implicitly.
func ArchiveConfigFromEnv() (*TerminalArchiveConfig, error) {
	names := archiveEnvironmentNames
	values := make([]string, len(names))
	set := 0
	for i, name := range names {
		values[i] = os.Getenv(name)
		if values[i] != "" {
			set++
		}
	}
	if set == 0 {
		return nil, nil
	}
	if set != len(names) {
		return nil, errors.New("GameFleet archive settings must all be explicitly configured")
	}
	info, err := os.Lstat(values[1])
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4097 {
		return nil, errors.New("GAMEFLEET_ARCHIVE_KEY_FILE must be a private regular file (0600 or 0400)")
	}
	f, err := os.Open(values[1])
	if err != nil {
		return nil, errors.New("cannot read GAMEFLEET_ARCHIVE_KEY_FILE")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 4098))
	if err != nil || len(raw) > 4097 {
		return nil, errors.New("invalid GameFleet archive key file")
	}
	return &TerminalArchiveConfig{TLS: tlsFilesFromEnv("GAMEFLEET_ARCHIVE"), URL: values[0], Key: strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r"),
		ApplicationID: values[2], IdentityIssuer: values[3], Region: values[4], Compatibility: values[5], ServiceID: values[6]}, nil
}

var archiveEnvironmentNames = []string{"GAMEFLEET_ARCHIVE_URL", "GAMEFLEET_ARCHIVE_KEY_FILE", "GAMEFLEET_ARCHIVE_APPLICATION_ID", "GAMEFLEET_ARCHIVE_IDENTITY_ISSUER", "GAMEFLEET_ARCHIVE_REGION", "GAMEFLEET_ARCHIVE_COMPATIBILITY", "GAMEFLEET_ARCHIVE_SERVICE_ID"}
