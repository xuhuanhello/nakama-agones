//go:build linux || darwin

package console

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func fileIdentity(info os.FileInfo) (uid, gid int, links uint64, ok bool) {
	value, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, false
	}
	return int(value.Uid), int(value.Gid), uint64(value.Nlink), true
}

func readPrivateFile(path string, max int64, owner int) ([]byte, os.FileInfo, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, errors.New("private_file_unavailable")
	}
	f := os.NewFile(uintptr(fd), "private-file")
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !privateFile(info) || info.Size() > max {
		return nil, nil, errors.New("unsafe_private_file")
	}
	uid, _, links, ok := fileIdentity(info)
	if !ok || links != 1 || (owner >= 0 && uid != owner) {
		return nil, nil, errors.New("unsafe_private_file")
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(data)) > max {
		return nil, nil, errors.New("private_file_size_limit")
	}
	return data, info, nil
}

func replacePrivateConfig(path string, before []byte, original os.FileInfo, replacement []byte) error {
	uid, gid, _, ok := fileIdentity(original)
	if !ok {
		return errors.New("cannot preserve configuration ownership")
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".fleet-console-config-*")
	if err != nil {
		return errors.New("cannot create private configuration replacement")
	}
	name := temp.Name()
	defer os.Remove(name)
	defer temp.Close()
	if err = temp.Chown(uid, gid); err != nil {
		return errors.New("cannot preserve configuration ownership")
	}
	if err = temp.Chmod(0600); err != nil {
		return errors.New("cannot preserve configuration permissions")
	}
	if _, err = temp.Write(replacement); err != nil {
		return errors.New("cannot write private configuration")
	}
	if err = temp.Sync(); err != nil {
		return errors.New("cannot sync private configuration")
	}
	if err = temp.Close(); err != nil {
		return errors.New("cannot close private configuration")
	}
	current, info, err := readPrivateFile(path, configSizeLimit, -1)
	if err != nil || !os.SameFile(original, info) || !bytes.Equal(current, before) {
		return errors.New("configuration changed during password update")
	}
	if err = os.Rename(name, path); err != nil {
		return errors.New("cannot replace private configuration")
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return errors.New("cannot sync configuration directory")
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return errors.New("cannot sync configuration directory")
	}
	return nil
}

func verifyControlDirectory(path string, owner int) (os.FileInfo, error) {
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil || !info.IsDir() || info.Mode().Perm()&0027 != 0 {
		return nil, errors.New("unsafe control socket directory")
	}
	uid, _, _, ok := fileIdentity(info)
	if !ok || uid != owner {
		return nil, errors.New("unsafe control socket directory")
	}
	return info, nil
}

func verifyControlSocket(path string, owner int) error {
	if _, err := verifyControlDirectory(path, owner); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0660 {
		return errors.New("unsafe control socket")
	}
	uid, _, _, ok := fileIdentity(info)
	if !ok || uid != owner {
		return errors.New("unsafe control socket owner")
	}
	return nil
}

func listenControlSocket(path string, owner int) (*net.UnixListener, error) {
	if !validSocketPath(path) || os.Geteuid() != owner {
		return nil, errors.New("invalid control socket identity")
	}
	directory, err := verifyControlDirectory(path, owner)
	if err != nil {
		return nil, err
	}
	if _, err = os.Lstat(path); err == nil {
		if err = verifyControlSocket(path, owner); err != nil {
			return nil, err
		}
		connection, err := net.DialTimeout("unix", path, 200*time.Millisecond)
		if err == nil {
			connection.Close()
			return nil, errors.New("control socket already in use")
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			return nil, errors.New("cannot verify stale control socket")
		}
		if err = os.Remove(path); err != nil {
			return nil, errors.New("cannot remove stale control socket")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("cannot inspect control socket")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, errors.New("cannot listen on control socket")
	}
	_, gid, _, _ := fileIdentity(directory)
	if err = os.Chown(path, owner, gid); err == nil {
		err = os.Chmod(path, 0660)
	}
	if err != nil {
		listener.Close()
		return nil, errors.New("cannot protect control socket")
	}
	return listener, nil
}
