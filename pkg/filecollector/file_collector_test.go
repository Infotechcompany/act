package filecollector

import (
	"archive/tar"
	"context"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type memoryFs struct {
	billy.Filesystem
}

func (mfs *memoryFs) walk(root string, fn filepath.WalkFunc) error {
	dir, err := mfs.ReadDir(root)
	if err != nil {
		return err
	}
	for i := 0; i < len(dir); i++ {
		filename := filepath.Join(root, dir[i].Name())
		err = fn(filename, dir[i], nil)
		if dir[i].IsDir() {
			if err == filepath.SkipDir {
				err = nil
			} else if err := mfs.walk(filename, fn); err != nil {
				return err
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (mfs *memoryFs) Walk(root string, fn filepath.WalkFunc) error {
	stat, err := mfs.Lstat(root)
	if err != nil {
		return err
	}
	err = fn(strings.Join([]string{root, "."}, string(filepath.Separator)), stat, nil)
	if err != nil {
		return err
	}
	return mfs.walk(root, fn)
}

func (mfs *memoryFs) OpenGitIndex(path string) (*index.Index, error) {
	f, _ := mfs.Filesystem.Chroot(filepath.Join(path, ".git"))
	storage := filesystem.NewStorage(f, cache.NewObjectLRUDefault())
	i, err := storage.Index()
	if err != nil {
		return nil, err
	}
	return i, nil
}

func (mfs *memoryFs) Open(path string) (io.ReadCloser, error) {
	return mfs.Filesystem.Open(path)
}

func (mfs *memoryFs) Readlink(path string) (string, error) {
	return mfs.Filesystem.Readlink(path)
}

func TestIgnoredTrackedfile(t *testing.T) {
	fs := memfs.New()
	_ = fs.MkdirAll("mygitrepo/.git", 0o777)
	dotgit, _ := fs.Chroot("mygitrepo/.git")
	worktree, _ := fs.Chroot("mygitrepo")
	repo, _ := git.Init(filesystem.NewStorage(dotgit, cache.NewObjectLRUDefault()), worktree)
	f, _ := worktree.Create(".gitignore")
	_, _ = f.Write([]byte(".*\n"))
	f.Close()
	// This file shouldn't be in the tar
	f, _ = worktree.Create(".env")
	_, _ = f.Write([]byte("test=val1\n"))
	f.Close()
	w, _ := repo.Worktree()
	// .gitignore is in the tar after adding it to the index
	_, _ = w.Add(".gitignore")

	tmpTar, _ := fs.Create("temp.tar")
	tw := tar.NewWriter(tmpTar)
	ps, _ := gitignore.ReadPatterns(worktree, []string{})
	ignorer := gitignore.NewMatcher(ps)
	fc := &FileCollector{
		Fs:        &memoryFs{Filesystem: fs},
		Ignorer:   ignorer,
		SrcPath:   "mygitrepo",
		SrcPrefix: "mygitrepo" + string(filepath.Separator),
		Handler: &TarCollector{
			TarWriter: tw,
		},
	}
	err := fc.Fs.Walk("mygitrepo", fc.CollectFiles(context.Background(), []string{}))
	assert.NoError(t, err, "successfully collect files")
	tw.Close()
	_, _ = tmpTar.Seek(0, io.SeekStart)
	tr := tar.NewReader(tmpTar)
	h, err := tr.Next()
	assert.NoError(t, err, "tar must not be empty")
	assert.Equal(t, ".gitignore", h.Name)
	_, err = tr.Next()
	assert.ErrorIs(t, err, io.EOF, "tar must only contain one element")
}

func TestSymlinks(t *testing.T) {
	fs := memfs.New()
	_ = fs.MkdirAll("mygitrepo/.git", 0o777)
	dotgit, _ := fs.Chroot("mygitrepo/.git")
	worktree, _ := fs.Chroot("mygitrepo")
	repo, _ := git.Init(filesystem.NewStorage(dotgit, cache.NewObjectLRUDefault()), worktree)
	// This file shouldn't be in the tar
	f, err := worktree.Create(".env")
	assert.NoError(t, err)
	_, err = f.Write([]byte("test=val1\n"))
	assert.NoError(t, err)
	f.Close()
	err = worktree.Symlink(".env", "test.env")
	assert.NoError(t, err)

	w, err := repo.Worktree()
	assert.NoError(t, err)

	// .gitignore is in the tar after adding it to the index
	_, err = w.Add(".env")
	assert.NoError(t, err)
	_, err = w.Add("test.env")
	assert.NoError(t, err)

	tmpTar, _ := fs.Create("temp.tar")
	tw := tar.NewWriter(tmpTar)
	ps, _ := gitignore.ReadPatterns(worktree, []string{})
	ignorer := gitignore.NewMatcher(ps)
	fc := &FileCollector{
		Fs:        &memoryFs{Filesystem: fs},
		Ignorer:   ignorer,
		SrcPath:   "mygitrepo",
		SrcPrefix: "mygitrepo" + string(filepath.Separator),
		Handler: &TarCollector{
			TarWriter: tw,
		},
	}
	err = fc.Fs.Walk("mygitrepo", fc.CollectFiles(context.Background(), []string{}))
	assert.NoError(t, err, "successfully collect files")
	tw.Close()
	_, _ = tmpTar.Seek(0, io.SeekStart)
	tr := tar.NewReader(tmpTar)
	h, err := tr.Next()
	files := map[string]tar.Header{}
	for err == nil {
		files[h.Name] = *h
		h, err = tr.Next()
	}

	assert.Equal(t, ".env", files[".env"].Name)
	assert.Equal(t, "test.env", files["test.env"].Name)
	assert.Equal(t, ".env", files["test.env"].Linkname)
	assert.ErrorIs(t, err, io.EOF, "tar must be read cleanly to EOF")
}

func TestCopyCollectorOverwritesReadOnlyFile(t *testing.T) {
	destDir := t.TempDir()
	destPath := filepath.Join(destDir, "objects", "pack", "pack.idx")
	require.NoError(t, os.MkdirAll(filepath.Dir(destPath), 0o755))
	require.NoError(t, os.WriteFile(destPath, []byte("old content"), 0o600))
	require.NoError(t, os.Chmod(destPath, 0o444))

	info, err := os.Stat(destPath)
	require.NoError(t, err)
	collector := &CopyCollector{DstDir: destDir}
	require.NoError(t, collector.WriteFile(filepath.Join("objects", "pack", "pack.idx"), info, "", strings.NewReader("new")))

	content, err := os.ReadFile(destPath)
	require.NoError(t, err)
	assert.Equal(t, "new", string(content))
	info, err = os.Stat(destPath)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o444), info.Mode().Perm())
}

type fileInfoWithMode struct {
	fs.FileInfo
	mode fs.FileMode
}

func (fi fileInfoWithMode) Mode() fs.FileMode {
	return fi.mode
}

func copyCollectorSourceInfo(t *testing.T) fs.FileInfo {
	t.Helper()
	sourcePath := filepath.Join(t.TempDir(), "source")
	require.NoError(t, os.WriteFile(sourcePath, []byte("source"), 0o600))
	info, err := os.Stat(sourcePath)
	require.NoError(t, err)
	return info
}

func TestCopyCollectorRejectsPathsOutsideDestination(t *testing.T) {
	parent := t.TempDir()
	destDir := filepath.Join(parent, "destination")
	collector := &CopyCollector{DstDir: destDir}
	info := copyCollectorSourceInfo(t)

	outsideRelative := filepath.Join("..", "outside-relative")
	require.Error(t, collector.WriteFile(outsideRelative, info, "", strings.NewReader("new")))
	_, err := os.Stat(filepath.Join(parent, "outside-relative"))
	require.ErrorIs(t, err, fs.ErrNotExist)

	outsideAbsolute := filepath.Join(parent, "outside-absolute")
	require.Error(t, collector.WriteFile(outsideAbsolute, info, "", strings.NewReader("new")))
	_, err = os.Stat(outsideAbsolute)
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestCopyCollectorRejectsEscapingSymlinkParent(t *testing.T) {
	parent := t.TempDir()
	destDir := filepath.Join(parent, "destination")
	outsideDir := filepath.Join(parent, "outside")
	require.NoError(t, os.MkdirAll(destDir, 0o755))
	require.NoError(t, os.MkdirAll(outsideDir, 0o755))
	if err := os.Symlink(filepath.Join("..", "outside"), filepath.Join(destDir, "pivot")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	collector := &CopyCollector{DstDir: destDir}
	err := collector.WriteFile(filepath.Join("pivot", "escaped"), copyCollectorSourceInfo(t), "", strings.NewReader("new"))
	require.Error(t, err)
	_, err = os.Stat(filepath.Join(outsideDir, "escaped"))
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestCopyCollectorRejectsInRootSymlinkParentAlias(t *testing.T) {
	parent := t.TempDir()
	destDir := filepath.Join(parent, "destination")
	require.NoError(t, os.MkdirAll(filepath.Join(destDir, "b"), 0o755))
	if err := os.Symlink(".", filepath.Join(destDir, "a")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	collector := &CopyCollector{DstDir: destDir}
	err := collector.WriteFile(filepath.Join("a", "b", "link"), copyCollectorSourceInfo(t), filepath.Join("..", "..", "outside"), nil)
	require.Error(t, err)
	_, err = os.Lstat(filepath.Join(destDir, "b", "link"))
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestCopyCollectorRejectsEscapingSymlinkTarget(t *testing.T) {
	destDir := t.TempDir()
	collector := &CopyCollector{DstDir: destDir}
	for _, target := range []string{
		filepath.Join("..", "..", "outside"),
		string(filepath.Separator) + "outside",
	} {
		err := collector.WriteFile(filepath.Join("nested", "link"), copyCollectorSourceInfo(t), target, nil)
		require.Error(t, err)
		_, err = os.Lstat(filepath.Join(destDir, "nested", "link"))
		require.ErrorIs(t, err, fs.ErrNotExist)
	}
}

func TestCopyCollectorSanitizesModeViaOpenFile(t *testing.T) {
	destDir := t.TempDir()
	collector := &CopyCollector{DstDir: destDir}
	info := copyCollectorSourceInfo(t)
	untrusted := fileInfoWithMode{
		FileInfo: info,
		mode:     fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky | 0o777,
	}
	require.NoError(t, collector.WriteFile("copied", untrusted, "", strings.NewReader("new")))

	copied, err := os.Stat(filepath.Join(destDir, "copied"))
	require.NoError(t, err)
	assert.Zero(t, copied.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky))
	if runtime.GOOS != "windows" {
		assert.Equal(t, fs.FileMode(0o755), copied.Mode().Perm())
	}
}

func TestCopyCollectorReplacesLeafSymlinkWithoutFollowingIt(t *testing.T) {
	parent := t.TempDir()
	destDir := filepath.Join(parent, "destination")
	require.NoError(t, os.MkdirAll(destDir, 0o755))
	outsidePath := filepath.Join(parent, "outside")
	require.NoError(t, os.WriteFile(outsidePath, []byte("outside"), 0o600))
	if err := os.Symlink(outsidePath, filepath.Join(destDir, "copied")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	collector := &CopyCollector{DstDir: destDir}
	require.NoError(t, collector.WriteFile("copied", copyCollectorSourceInfo(t), "", strings.NewReader("new")))
	content, err := os.ReadFile(outsidePath)
	require.NoError(t, err)
	assert.Equal(t, "outside", string(content))
	content, err = os.ReadFile(filepath.Join(destDir, "copied"))
	require.NoError(t, err)
	assert.Equal(t, "new", string(content))
	info, err := os.Lstat(filepath.Join(destDir, "copied"))
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular())
}

func TestCopyCollectorDoesNotReplaceDirectory(t *testing.T) {
	destDir := t.TempDir()
	destPath := filepath.Join(destDir, "copied")
	require.NoError(t, os.Mkdir(destPath, 0o700))

	collector := &CopyCollector{DstDir: destDir}
	require.Error(t, collector.WriteFile("copied", copyCollectorSourceInfo(t), "", strings.NewReader("new")))
	info, err := os.Stat(destPath)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestCopyCollectorDoesNotReplaceSpecialFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets are not available on Windows")
	}
	destDir := t.TempDir()
	destPath := filepath.Join(destDir, "copied")
	listener, err := net.Listen("unix", destPath)
	if err != nil {
		t.Skipf("Unix sockets unavailable: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	collector := &CopyCollector{DstDir: destDir}
	require.Error(t, collector.WriteFile("copied", copyCollectorSourceInfo(t), "", strings.NewReader("new")))
	info, err := os.Lstat(destPath)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&fs.ModeSocket)
}
