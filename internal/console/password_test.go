package console

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadAPITokenIsRandomHashedAndBounded(t *testing.T) {
	one, hash, err := GenerateReadAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	two, _, _ := GenerateReadAPIToken()
	if one == two || len(one) != 48 || !verifyReadAPIToken(one, hash) || verifyReadAPIToken(two, hash) || verifyReadAPIToken(one, strings.ToUpper(hash)) {
		t.Fatal("read token generation or verification is unsafe")
	}
	for _, value := range []string{"plain-token", "sha256:short", strings.Repeat("x", 71)} {
		cfg := testConfig(t)
		cfg.ReadAPITokenHash = value
		if cfg.validate() == nil {
			t.Fatal("invalid API hash accepted")
		}
	}
}

func TestSetPasswordPreservesConfigurationOwnershipAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := testConfig(t)
	_, cfg.ReadAPITokenHash, _ = GenerateReadAPIToken()
	raw, _ := json.Marshal(cfg)
	if os.WriteFile(path, raw, 0600) != nil {
		t.Fatal("cannot prepare config")
	}
	before, _ := os.Stat(path)
	uid, gid, _, _ := fileIdentity(before)
	if err := SetPassword(path, "new administrator password"); err != nil {
		t.Fatal(err)
	}
	after, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	newUID, newGID, _, _ := fileIdentity(info)
	if !VerifyPassword("new administrator password", after.PasswordHash) || VerifyPassword(testPassword, after.PasswordHash) || after.Username != cfg.Username || after.ReadAPITokenHash != cfg.ReadAPITokenHash || after.PublicURL != cfg.PublicURL || info.Mode().Perm() != 0600 || uid != newUID || gid != newGID || os.SameFile(before, info) {
		t.Fatal("password replacement changed deployment settings/identity or was not atomic")
	}
	stable, _ := os.ReadFile(path)
	if SetPassword(path, "short") == nil {
		t.Fatal("short replacement password accepted")
	}
	current, _ := os.ReadFile(path)
	if !bytes.Equal(stable, current) {
		t.Fatal("failed password replacement modified config")
	}
	link := path + ".link"
	os.Symlink(path, link)
	if SetPassword(link, "another administrator password") == nil {
		t.Fatal("symlink configuration updated")
	}
}

func TestPrivateReplacementRefusesConcurrentConfigurationChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte("original"), 0600)
	before, info, err := readPrivateFile(path, 100, -1)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte("changed by operator"), 0600)
	if replacePrivateConfig(path, before, info, []byte("replacement")) == nil {
		t.Fatal("concurrent config change was overwritten")
	}
	current, _ := os.ReadFile(path)
	if string(current) != "changed by operator" {
		t.Fatal("concurrent operator config was not preserved")
	}
}
