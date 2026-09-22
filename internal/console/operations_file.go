package console

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const operationsLimit = 2 << 20

func openOperations(path string) (*operations, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("invalid operations file")
	}
	dir, err := os.Lstat(filepath.Dir(path))
	if err != nil || !dir.IsDir() || dir.Mode().Perm() != 0700 {
		return nil, errors.New("operations directory must be private 0700")
	}
	uid, _, _, ok := fileIdentity(dir)
	if !ok || uid != os.Geteuid() {
		return nil, errors.New("operations directory owner differs")
	}
	fd, err := syscall.Open(path+".lock", syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, errors.New("operations lock unavailable")
	}
	lock := os.NewFile(uintptr(fd), "operations-lock")
	info, err := lock.Stat()
	if err != nil {
		lock.Close()
		return nil, errors.New("operations lock unavailable")
	}
	owner, _, links, safe := fileIdentity(info)
	if !privateFile(info) || !safe || links != 1 || owner != os.Geteuid() || syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		lock.Close()
		return nil, errors.New("operations already in use or unsafe")
	}
	o := &operations{path: path, closeFile: func() { lock.Close() }, sender: newFeishuSender(), now: time.Now}
	if _, err = os.Lstat(path); os.IsNotExist(err) {
		initial := operationsState{Schema: 1, Revision: 1, Config: defaultAlerts(), Active: map[string]*alertCondition{}}
		raw, _ := json.Marshal(initial)
		f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			o.closeFile()
			return nil, errors.New("cannot create operations file")
		}
		_, e = f.Write(raw)
		if e == nil {
			e = f.Sync()
		}
		f.Close()
		if e != nil {
			o.closeFile()
			return nil, errors.New("cannot initialize operations file")
		}
	}
	raw, _, err := readPrivateFile(path, operationsLimit, os.Geteuid())
	if err != nil {
		o.closeFile()
		return nil, errors.New("invalid private operations file")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&o.state) != nil || d.Decode(new(any)) != io.EOF || o.state.Schema != 1 || o.state.Revision < 1 || !o.state.Config.valid() || !validFeishuWebhook(o.state.FeishuWebhook) || !validFeishuSecret(o.state.FeishuSigningSecret) || len(o.state.Active) > 5000 || len(o.state.History) > 200 || len(o.state.Audit) > 100 {
		o.closeFile()
		return nil, errors.New("invalid operations configuration")
	}
	if o.state.Active == nil {
		o.state.Active = map[string]*alertCondition{}
	}
	for key, entry := range o.state.Active {
		if entry == nil || entry.Key != key || len(key) > 256 || !finite(entry.Value) || !finite(entry.Threshold) {
			o.closeFile()
			return nil, errors.New("invalid operations state")
		}
	}
	if o.state.Config.FeishuEnabled && o.state.FeishuWebhook == "" {
		o.closeFile()
		return nil, errors.New("enabled Feishu delivery requires configuration")
	}
	o.diskHash = sha256.Sum256(raw)
	return o, nil
}
func (o *operations) save(next operationsState) error {
	raw, err := json.Marshal(next)
	if err != nil || len(raw) > operationsLimit {
		return sourceError{503, "alerts_storage_unavailable"}
	}
	if o.path == "" {
		o.state = next
		return nil
	} // In-memory engine fixture only.
	current, _, err := readPrivateFile(o.path, operationsLimit, os.Geteuid())
	if err != nil || sha256.Sum256(current) != o.diskHash {
		return sourceError{409, "alerts_file_changed"}
	}
	f, err := os.CreateTemp(filepath.Dir(o.path), ".operations-*")
	if err != nil {
		return sourceError{503, "alerts_storage_unavailable"}
	}
	name := f.Name()
	defer os.Remove(name)
	defer f.Close()
	if f.Chmod(0600) != nil {
		return sourceError{503, "alerts_storage_unavailable"}
	}
	if _, err = f.Write(raw); err != nil {
		return sourceError{503, "alerts_storage_unavailable"}
	}
	if f.Sync() != nil || f.Close() != nil {
		return sourceError{503, "alerts_storage_unavailable"}
	}
	if os.Rename(name, o.path) != nil {
		return sourceError{503, "alerts_storage_unavailable"}
	}
	// Rename is the publication point; retain in-memory parity even if the
	// subsequent directory fsync fails, then surface a stable durability error.
	o.diskHash = sha256.Sum256(raw)
	o.state = next
	dir, err := os.Open(filepath.Dir(o.path))
	if err != nil {
		return sourceError{503, "alerts_storage_unavailable"}
	}
	defer dir.Close()
	if dir.Sync() != nil {
		return sourceError{503, "alerts_storage_unavailable"}
	}
	return nil
}
