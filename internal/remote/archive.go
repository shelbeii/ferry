package remote

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/pkg/sftp"
)

// CompressFiles writes a ZIP next to the selected files and publishes it after it is complete.
func (c *FileClient) CompressFiles(ctx context.Context, sources []string, destination string, progress func(string)) error {
	s, err := c.sftpClient()
	if err != nil {
		return err
	}
	dst := path.Clean(destination)
	err = writeRemoteZIP(ctx, s, sources, dst, progress)
	if ctx.Err() != nil && err != nil {
		return ErrCanceled
	}
	return fileErr("压缩", dst, err)
}

func writeRemoteZIP(ctx context.Context, s *sftp.Client, sources []string, dst string, progress func(string)) error {
	if len(sources) == 0 || !strings.EqualFold(path.Ext(dst), ".zip") {
		return errors.New("请选择要压缩的项目并指定 .zip 文件名")
	}
	if err := archiveDestination(s, dst); err != nil {
		return err
	}
	tmp := path.Join(path.Dir(dst), ".ferry-archive-"+rand.Text())
	out, err := s.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return err
	}
	defer s.Remove(tmp)
	defer out.Close()
	stop := context.AfterFunc(ctx, func() { out.Close() })
	defer stop()
	if err := out.Chmod(0o600); err != nil {
		return err
	}
	buffer := bufio.NewWriterSize(out, 256<<10)
	z := zip.NewWriter(buffer)
	added := make(map[string]bool, len(sources))
	for _, src := range sources {
		src = path.Clean(src)
		name := path.Base(src)
		if path.Dir(src) != path.Dir(dst) || src == dst || added[name] {
			return errors.New("请选择同一目录中的不同项目，压缩包应保存在同一目录")
		}
		added[name] = true
		info, err := s.Lstat(src)
		if err != nil {
			return err
		}
		if err := addRemoteZIPEntry(ctx, s, z, src, name, info, progress); err != nil {
			return err
		}
	}
	if err := z.Close(); err != nil {
		return err
	}
	if err := buffer.Flush(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return publishArchive(ctx, s, tmp, dst)
}

func addRemoteZIPEntry(ctx context.Context, s *sftp.Client, z *zip.Writer, src, name string, info fs.FileInfo, progress func(string)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !archiveEntryName(name) || (!info.IsDir() && !info.Mode().IsRegular()) {
		return fmt.Errorf("压缩包不支持此文件类型或名称：%s", name)
	}
	h, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	h.Name = name
	if info.IsDir() {
		h.Name += "/"
	} else {
		h.Method = zip.Deflate
	}
	dst, err := z.CreateHeader(h)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		progress(name)
		in, err := s.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		stop := context.AfterFunc(ctx, func() { in.Close() })
		defer stop()
		_, err = io.Copy(dst, in)
		return err
	}
	children, err := s.ReadDirContext(ctx, src)
	if err != nil {
		return err
	}
	for _, child := range children {
		if !archiveEntryName(child.Name()) || strings.Contains(child.Name(), "/") {
			return fmt.Errorf("不支持的文件名：%s", child.Name())
		}
		if err := addRemoteZIPEntry(ctx, s, z, path.Join(src, child.Name()), name+"/"+child.Name(), child, progress); err != nil {
			return err
		}
	}
	return nil
}

// ExtractArchive expands an archive into a new directory beside it.
func (c *FileClient) ExtractArchive(ctx context.Context, source, destination string, progress func(string)) error {
	s, err := c.sftpClient()
	if err != nil {
		return err
	}
	err = extractRemoteArchive(ctx, s, path.Clean(source), path.Clean(destination), progress)
	if ctx.Err() != nil && err != nil {
		return ErrCanceled
	}
	return fileErr("解压", source, err)
}

func extractRemoteArchive(ctx context.Context, s *sftp.Client, src, dst string, progress func(string)) error {
	name := strings.ToLower(src)
	isZIP := strings.HasSuffix(name, ".zip")
	if !isZIP && !strings.HasSuffix(name, ".tar.gz") && !strings.HasSuffix(name, ".tgz") {
		return errors.New("仅支持 .zip、.tar.gz、.tgz 压缩包")
	}
	if err := archiveDestination(s, dst); err != nil {
		return err
	}
	info, err := s.Lstat(src)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("请选择普通压缩文件")
	}
	in, err := s.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	stop := context.AfterFunc(ctx, func() { in.Close() })
	defer stop()
	tmp := path.Join(path.Dir(dst), ".ferry-extract-"+rand.Text())
	if err := s.Mkdir(tmp); err != nil {
		return err
	}
	defer s.RemoveAll(tmp)
	if err := s.Chmod(tmp, 0o700); err != nil {
		return err
	}
	write := func(raw string, mode fs.FileMode, contents io.Reader) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := strings.TrimSuffix(strings.TrimPrefix(raw, "./"), "/")
		if mode.IsDir() && (name == "." || raw == "./") {
			return nil
		}
		if !archiveEntryName(name) || (!mode.IsDir() && !mode.IsRegular()) {
			return fmt.Errorf("压缩包包含不支持的路径或文件类型：%s", raw)
		}
		out := path.Join(tmp, name)
		if mode.IsDir() {
			return s.MkdirAll(out)
		}
		if err := s.MkdirAll(path.Dir(out)); err != nil {
			return err
		}
		progress(name)
		f, err := s.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
		if err != nil {
			return err
		}
		stop := context.AfterFunc(ctx, func() { f.Close() })
		defer stop()
		_, err = io.Copy(f, contents)
		if err == nil {
			err = f.Chmod(0o600 | (mode.Perm() & 0o100))
		}
		return errors.Join(err, f.Close())
	}
	if isZIP {
		z, err := zip.NewReader(in, info.Size())
		if err != nil {
			return err
		}
		for _, entry := range z.File {
			if err := readZIPEntry(entry, write); err != nil {
				return err
			}
		}
	} else if err := readTarGzip(in, write); err != nil {
		return err
	}
	return publishArchive(ctx, s, tmp, dst)
}

func readZIPEntry(entry *zip.File, write func(string, fs.FileMode, io.Reader) error) error {
	mode := entry.Mode()
	if mode.IsDir() || !mode.IsRegular() {
		return write(entry.Name, mode, nil)
	}
	in, err := entry.Open()
	if err != nil {
		return err
	}
	defer in.Close()
	return write(entry.Name, mode, in)
}

func readTarGzip(in io.Reader, write func(string, fs.FileMode, io.Reader) error) error {
	gz, err := gzip.NewReader(in)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			// Consume the rest of the stream so gzip can verify its checksum.
			_, err = io.Copy(io.Discard, gz)
			return err
		}
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir, tar.TypeReg:
			if err := write(h.Name, h.FileInfo().Mode(), tr); err != nil {
				return err
			}
		default:
			return fmt.Errorf("压缩包包含不支持的文件类型：%s", h.Name)
		}
	}
}

func archiveEntryName(name string) bool {
	return name != "." && fs.ValidPath(name) && !strings.ContainsAny(name, "\\:\x00")
}

func archiveDestination(s *sftp.Client, dst string) error {
	if _, err := s.Lstat(dst); !errors.Is(err, fs.ErrNotExist) {
		if err != nil {
			return err
		}
		return fmt.Errorf("%s 已经存在，请使用其他名字", dst)
	}
	return nil
}

var archivePublish sync.Mutex

func publishArchive(ctx context.Context, s *sftp.Client, temporary, destination string) error {
	archivePublish.Lock()
	defer archivePublish.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := archiveDestination(s, destination); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Use the SFTP rename that rejects an existing destination.
	return s.Rename(temporary, destination)
}
