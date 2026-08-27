package container

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Type assert HostEnvironment implements ExecutionsEnvironment
var _ ExecutionsEnvironment = &HostEnvironment{}

type cancelAfterReader struct {
	reader    *bytes.Reader
	cancel    context.CancelFunc
	threshold int
	read      int
}

func (r *cancelAfterReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += n
	if r.read >= r.threshold {
		r.cancel()
	}
	return n, err
}

func TestCopyDir(t *testing.T) {
	dir, err := os.MkdirTemp("", "test-host-env-*")
	assert.NoError(t, err)
	defer os.RemoveAll(dir)
	ctx := context.Background()
	e := &HostEnvironment{
		Path:      filepath.Join(dir, "path"),
		TmpDir:    filepath.Join(dir, "tmp"),
		ToolCache: filepath.Join(dir, "tool_cache"),
		ActPath:   filepath.Join(dir, "act_path"),
		StdOut:    os.Stdout,
		Workdir:   path.Join("testdata", "scratch"),
	}
	_ = os.MkdirAll(e.Path, 0700)
	_ = os.MkdirAll(e.TmpDir, 0700)
	_ = os.MkdirAll(e.ToolCache, 0700)
	_ = os.MkdirAll(e.ActPath, 0700)
	err = e.CopyDir(e.Workdir, e.Path, true)(ctx)
	assert.NoError(t, err)
}

func TestGetContainerArchive(t *testing.T) {
	dir, err := os.MkdirTemp("", "test-host-env-*")
	assert.NoError(t, err)
	defer os.RemoveAll(dir)
	ctx := context.Background()
	e := &HostEnvironment{
		Path:      filepath.Join(dir, "path"),
		TmpDir:    filepath.Join(dir, "tmp"),
		ToolCache: filepath.Join(dir, "tool_cache"),
		ActPath:   filepath.Join(dir, "act_path"),
		StdOut:    os.Stdout,
		Workdir:   path.Join("testdata", "scratch"),
	}
	_ = os.MkdirAll(e.Path, 0700)
	_ = os.MkdirAll(e.TmpDir, 0700)
	_ = os.MkdirAll(e.ToolCache, 0700)
	_ = os.MkdirAll(e.ActPath, 0700)
	expectedContent := []byte("sdde/7sh")
	err = os.WriteFile(filepath.Join(e.Path, "action.yml"), expectedContent, 0600)
	assert.NoError(t, err)
	archive, err := e.GetContainerArchive(ctx, e.Path)
	assert.NoError(t, err)
	defer archive.Close()
	reader := tar.NewReader(archive)
	h, err := reader.Next()
	assert.NoError(t, err)
	assert.Equal(t, "action.yml", h.Name)
	content, err := io.ReadAll(reader)
	assert.NoError(t, err)
	assert.Equal(t, expectedContent, content)
	_, err = reader.Next()
	assert.ErrorIs(t, err, io.EOF)
}

func reverseOrderedSymlinkArchive(t *testing.T) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	assert.NoError(t, writer.WriteHeader(&tar.Header{
		Name:     "b",
		Linkname: "a/../outside",
		Mode:     0o777,
		Typeflag: tar.TypeSymlink,
	}))
	assert.NoError(t, writer.WriteHeader(&tar.Header{
		Name:     "a",
		Linkname: ".",
		Mode:     0o777,
		Typeflag: tar.TypeSymlink,
	}))
	assert.NoError(t, writer.Close())
	return archive.Bytes()
}

func TestCopyTarStreamCleansStagedSymlinkAfterTruncatedArchive(t *testing.T) {
	archive := reverseOrderedSymlinkArchive(t)
	destPath := filepath.Join(t.TempDir(), "destination")
	// Preserve the first complete header and leave an incomplete second header.
	err := (&HostEnvironment{}).CopyTarStream(context.Background(), destPath, bytes.NewReader(archive[:512+100]))
	assert.Error(t, err)
	_, err = os.Lstat(destPath)
	assert.ErrorIs(t, err, fs.ErrNotExist)
}

func TestCopyTarStreamCleansStagedSymlinkAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	archive := reverseOrderedSymlinkArchive(t)
	reader := &cancelAfterReader{reader: bytes.NewReader(archive), cancel: cancel, threshold: 1024}
	destPath := filepath.Join(t.TempDir(), "destination")
	err := (&HostEnvironment{}).CopyTarStream(ctx, destPath, reader)
	assert.Error(t, err)
	_, err = os.Lstat(destPath)
	assert.ErrorIs(t, err, fs.ErrNotExist)
}

func TestCopyDirFinalizesAfterWalkError(t *testing.T) {
	srcPath := t.TempDir()
	destPath := filepath.Join(t.TempDir(), "destination")
	links := []struct{ name, target string }{
		{"1-b", "2-a/../outside"},
		{"2-a", "."},
		{"3-bad", "../../outside"},
	}
	for _, link := range links {
		if err := os.Symlink(link.target, filepath.Join(srcPath, link.name)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}

	err := (&HostEnvironment{}).CopyDir(destPath, srcPath, false)(context.Background())
	assert.Error(t, err)
	_, err = os.Lstat(filepath.Join(destPath, "1-b"))
	assert.ErrorIs(t, err, fs.ErrNotExist)
}
