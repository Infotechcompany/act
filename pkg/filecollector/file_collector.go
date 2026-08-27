package filecollector

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/go-git/go-git/v5/plumbing/format/index"
)

type Handler interface {
	WriteFile(path string, fi fs.FileInfo, linkName string, f io.Reader) error
}

type TarCollector struct {
	TarWriter *tar.Writer
	UID       int
	GID       int
	DstDir    string
}

func (tc TarCollector) WriteFile(fpath string, fi fs.FileInfo, linkName string, f io.Reader) error {
	// create a new dir/file header
	header, err := tar.FileInfoHeader(fi, linkName)
	if err != nil {
		return err
	}

	// update the name to correctly reflect the desired destination when untaring
	header.Name = path.Join(tc.DstDir, fpath)
	header.Mode = int64(fi.Mode())
	header.ModTime = fi.ModTime()
	header.Uid = tc.UID
	header.Gid = tc.GID

	// write the header
	if err := tc.TarWriter.WriteHeader(header); err != nil {
		return err
	}

	// this is a symlink no reader provided
	if f == nil {
		return nil
	}

	// copy file data into tar writer
	if _, err := io.Copy(tc.TarWriter, f); err != nil {
		return err
	}
	return nil
}

type CopyCollector struct {
	DstDir string
}

func createRootTempFile(root *os.Root) (*os.File, string, error) {
	var random [16]byte
	for range 100 {
		if _, err := rand.Read(random[:]); err != nil {
			return nil, "", err
		}
		name := ".act-copy-" + hex.EncodeToString(random[:])
		f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			return f, name, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", fmt.Errorf("unable to create temporary copy file")
}

func rollbackBackupCleanup(root *os.Root, tempName, destName, backupName string, cleanupErr error) error {
	if err := root.Rename(destName, tempName); err != nil {
		return errors.Join(cleanupErr, fmt.Errorf("move replacement aside for rollback: %w", err))
	}
	if restoreErr := root.Rename(backupName, destName); restoreErr != nil {
		reinstallErr := root.Rename(tempName, destName)
		if reinstallErr != nil {
			return errors.Join(cleanupErr, fmt.Errorf("restore original destination: %w", restoreErr),
				fmt.Errorf("reinstall replacement after failed rollback: %w", reinstallErr))
		}
		return errors.Join(cleanupErr, fmt.Errorf("restore original destination: %w", restoreErr))
	}
	return cleanupErr
}

func replaceRootEntry(root *os.Root, tempName, destName string) error {
	existing, statErr := root.Lstat(destName)
	if errors.Is(statErr, fs.ErrNotExist) {
		return root.Rename(tempName, destName)
	} else if statErr != nil {
		return statErr
	}
	if !existing.Mode().IsRegular() && existing.Mode()&fs.ModeSymlink == 0 {
		return fmt.Errorf("refusing to replace non-file destination %q", destName)
	}

	renameErr := root.Rename(tempName, destName)
	if renameErr == nil {
		return nil
	}
	if !errors.Is(renameErr, fs.ErrExist) && !errors.Is(renameErr, fs.ErrPermission) {
		return renameErr
	}

	// Windows cannot rename over some existing entries. Move the original to a
	// same-directory backup first, so an unexpected installation failure can be
	// rolled back without path-based chmod or deletion.
	backup, backupName, err := createRootTempFile(root)
	if err != nil {
		return err
	}
	if err := backup.Close(); err != nil {
		_ = root.Remove(backupName)
		return err
	}
	if err := root.Remove(backupName); err != nil {
		return err
	}
	if err := root.Rename(destName, backupName); err != nil {
		return err
	}
	if err := root.Rename(tempName, destName); err != nil {
		rollbackErr := root.Rename(backupName, destName)
		if rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("restore original destination: %w", rollbackErr))
		}
		return err
	}
	if cleanupErr := root.Remove(backupName); cleanupErr != nil {
		return rollbackBackupCleanup(root, tempName, destName, backupName, cleanupErr)
	}
	return nil
}

func ensureNoSymlinkParents(root *os.Root, parentName string) error {
	if parentName == "." {
		return nil
	}
	prefix := ""
	for _, component := range strings.Split(parentName, string(filepath.Separator)) {
		prefix = filepath.Join(prefix, component)
		info, err := root.Lstat(prefix)
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("copy destination parent %q is a symlink", prefix)
		}
		if !info.IsDir() {
			return fmt.Errorf("copy destination parent %q is not a directory", prefix)
		}
	}
	return nil
}

func hasParentPathComponent(name string) bool {
	for _, component := range strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' }) {
		if component == ".." {
			return true
		}
	}
	return false
}

func (cc *CopyCollector) WriteFile(fpath string, fi fs.FileInfo, linkName string, f io.Reader) error {
	if !filepath.IsLocal(fpath) || filepath.Clean(fpath) == "." {
		return fmt.Errorf("copy destination %q is not a local path", fpath)
	}
	fpath = filepath.Clean(fpath)
	if err := os.MkdirAll(cc.DstDir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(cc.DstDir)
	if err != nil {
		return err
	}
	defer root.Close()

	parentName := filepath.Dir(fpath)
	if err := root.MkdirAll(parentName, 0o755); err != nil {
		return err
	}
	if err := ensureNoSymlinkParents(root, parentName); err != nil {
		return err
	}
	parent, err := root.OpenRoot(parentName)
	if err != nil {
		return err
	}
	defer parent.Close()

	destName := filepath.Base(fpath)
	if linkName != "" {
		if filepath.IsAbs(linkName) || filepath.VolumeName(linkName) != "" || os.IsPathSeparator(linkName[0]) ||
			hasParentPathComponent(linkName) || !filepath.IsLocal(filepath.Join(parentName, linkName)) {
			return fmt.Errorf("symlink target %q escapes copy destination", linkName)
		}
		tempFile, tempName, err := createRootTempFile(parent)
		if err != nil {
			return err
		}
		if err := tempFile.Close(); err != nil {
			_ = parent.Remove(tempName)
			return err
		}
		if err := parent.Remove(tempName); err != nil {
			return err
		}
		if err := parent.Symlink(linkName, tempName); err != nil {
			return err
		}
		defer func() { _ = parent.Remove(tempName) }()
		// Resolve the link through the original root as a final confinement
		// check. ErrNotExist is safe for a dangling link, while an escaping
		// target produces a distinct os.Root traversal error.
		if _, err := root.Stat(filepath.Join(parentName, tempName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("validate symlink target %q: %w", linkName, err)
		}
		return replaceRootEntry(parent, tempName, destName)
	}

	df, tempName, err := createRootTempFile(parent)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Remove(tempName) }()
	if _, err := io.Copy(df, f); err != nil {
		_ = df.Close()
		return err
	}
	// Preserve read/execute intent while stripping special and group/other
	// write bits from untrusted archive modes. Apply it via the open descriptor
	// so a path substitution cannot redirect chmod.
	if err := df.Chmod(fi.Mode().Perm() & 0o755); err != nil {
		_ = df.Close()
		return err
	}
	if err := df.Close(); err != nil {
		return err
	}
	return replaceRootEntry(parent, tempName, destName)
}

type FileCollector struct {
	Ignorer   gitignore.Matcher
	SrcPath   string
	SrcPrefix string
	Fs        Fs
	Handler   Handler
}

type Fs interface {
	Walk(root string, fn filepath.WalkFunc) error
	OpenGitIndex(path string) (*index.Index, error)
	Open(path string) (io.ReadCloser, error)
	Readlink(path string) (string, error)
}

type DefaultFs struct {
}

func (*DefaultFs) Walk(root string, fn filepath.WalkFunc) error {
	return filepath.Walk(root, fn)
}

func (*DefaultFs) OpenGitIndex(path string) (*index.Index, error) {
	r, err := git.PlainOpen(path)
	if err != nil {
		return nil, err
	}
	i, err := r.Storer.Index()
	if err != nil {
		return nil, err
	}
	return i, nil
}

func (*DefaultFs) Open(path string) (io.ReadCloser, error) {
	return os.Open(path)
}

func (*DefaultFs) Readlink(path string) (string, error) {
	return os.Readlink(path)
}

//nolint:gocyclo
func (fc *FileCollector) CollectFiles(ctx context.Context, submodulePath []string) filepath.WalkFunc {
	i, _ := fc.Fs.OpenGitIndex(path.Join(fc.SrcPath, path.Join(submodulePath...)))
	return func(file string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if ctx != nil {
			select {
			case <-ctx.Done():
				return fmt.Errorf("copy cancelled")
			default:
			}
		}

		sansPrefix := strings.TrimPrefix(file, fc.SrcPrefix)
		split := strings.Split(sansPrefix, string(filepath.Separator))
		// The root folders should be skipped, submodules only have the last path component set to "." by filepath.Walk
		if fi.IsDir() && len(split) > 0 && split[len(split)-1] == "." {
			return nil
		}
		var entry *index.Entry
		if i != nil {
			entry, err = i.Entry(strings.Join(split[len(submodulePath):], "/"))
		} else {
			err = index.ErrEntryNotFound
		}
		if err != nil && fc.Ignorer != nil && fc.Ignorer.Match(split, fi.IsDir()) {
			if fi.IsDir() {
				if i != nil {
					ms, err := i.Glob(strings.Join(append(split[len(submodulePath):], "**"), "/"))
					if err != nil || len(ms) == 0 {
						return filepath.SkipDir
					}
				} else {
					return filepath.SkipDir
				}
			} else {
				return nil
			}
		}
		if err == nil && entry.Mode == filemode.Submodule {
			err = fc.Fs.Walk(file, fc.CollectFiles(ctx, split))
			if err != nil {
				return err
			}
			return filepath.SkipDir
		}
		path := filepath.ToSlash(sansPrefix)

		// return on non-regular files (thanks to [kumo](https://medium.com/@komuw/just-like-you-did-fbdd7df829d3) for this suggested update)
		if fi.Mode()&os.ModeSymlink == os.ModeSymlink {
			linkName, err := fc.Fs.Readlink(file)
			if err != nil {
				return fmt.Errorf("unable to readlink '%s': %w", file, err)
			}
			return fc.Handler.WriteFile(path, fi, linkName, nil)
		} else if !fi.Mode().IsRegular() {
			return nil
		}

		// open file
		f, err := fc.Fs.Open(file)
		if err != nil {
			return err
		}
		defer f.Close()

		if ctx != nil {
			// make io.Copy cancellable by closing the file
			cpctx, cpfinish := context.WithCancel(ctx)
			defer cpfinish()
			go func() {
				select {
				case <-cpctx.Done():
				case <-ctx.Done():
					f.Close()
				}
			}()
		}

		return fc.Handler.WriteFile(path, fi, "", f)
	}
}
