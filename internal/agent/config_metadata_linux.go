package agent

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func configMetadata(st os.FileInfo) (string, bool) {
	s := st.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%d:%d:%d:%d:%d:%d:%d", s.Dev, s.Ino, s.Ctim.Sec, s.Ctim.Nsec, s.Uid, s.Gid, s.Nlink), s.Nlink > 1
}

func configAttributes(f *os.File) (map[string]string, error) {
	fd := int(f.Fd())
	size, err := unix.Flistxattr(fd, nil)
	attrs := make(map[string]string)
	if errors.Is(err, unix.ENOTSUP) || (err == nil && size == 0) {
		return attrs, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]byte, size)
	n, err := unix.Flistxattr(fd, names)
	if err != nil {
		return nil, err
	}
	if n > len(names) {
		return nil, errors.New("读取期间文件扩展属性发生变化")
	}
	for _, name := range strings.Split(strings.TrimRight(string(names[:n]), "\x00"), "\x00") {
		if name == "" {
			continue
		}
		size, err := unix.Fgetxattr(fd, name, nil)
		if err != nil {
			return nil, err
		}
		value := make([]byte, size)
		n, err := unix.Fgetxattr(fd, name, value)
		if err != nil {
			return nil, err
		}
		if n > len(value) {
			return nil, errors.New("读取期间文件扩展属性发生变化")
		}
		attrs[name] = string(value[:n])
	}
	return attrs, nil
}

func preserveConfigMetadata(original, staged *os.File, inPlace bool) error {
	attrs, err := configAttributes(original)
	if err != nil {
		return err
	}
	if _, ok := attrs["security.capability"]; ok {
		return errors.New("文件带有 Linux capabilities，无法安全编辑")
	}
	if inPlace {
		return nil
	}
	current, err := configAttributes(staged)
	if err != nil {
		return err
	}
	for name := range current {
		if _, ok := attrs[name]; !ok {
			if err := unix.Fremovexattr(int(staged.Fd()), name); err != nil {
				return err
			}
		}
	}
	for name, value := range attrs {
		if old, ok := current[name]; ok && old == value {
			continue
		}
		if err := unix.Fsetxattr(int(staged.Fd()), name, []byte(value), 0); err != nil {
			return err
		}
	}
	result, err := configAttributes(staged)
	if err != nil {
		return err
	}
	before, err := original.Stat()
	if err != nil {
		return err
	}
	after, err := staged.Stat()
	if err != nil {
		return err
	}
	if !maps.Equal(attrs, result) || before.Mode() != after.Mode() {
		return errors.New("无法完整保留文件 ACL 或扩展属性")
	}
	return nil
}
