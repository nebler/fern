package taskstore

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	_ "modernc.org/sqlite"
)

const busyTimeoutMS = 5000

type Store struct {
	db   *sql.DB
	path string
}

// Open opens or creates dbPath. Its immediate parent must already exist, must
// not be a symlink, and must be private to the current user on permissioned
// platforms.
func Open(ctx context.Context, dbPath string) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := validateDBPath(dbPath)
	if err != nil {
		return nil, err
	}

	if _, err := os.Lstat(path); errorsIsNotExist(err) {
		f, createErr := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			return nil, fmt.Errorf("create task database: %w", createErr)
		}
		if closeErr := f.Close(); closeErr != nil {
			return nil, fmt.Errorf("close new task database: %w", closeErr)
		}
	} else if err != nil {
		return nil, fmt.Errorf("inspect task database: %w", err)
	}
	if err := validateDBFile(path); err != nil {
		return nil, err
	}

	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	q := u.Query()
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Set("_txlock", "immediate")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open task database: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)

	s := &Store{db: db, path: path}
	if err := s.initialize(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func errorsIsNotExist(err error) bool { return err != nil && os.IsNotExist(err) }

func validateDBPath(dbPath string) (string, error) {
	if dbPath == "" || strings.IndexByte(dbPath, 0) >= 0 {
		return "", fmt.Errorf("%w: empty database path", ErrUnsafePath)
	}
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnsafePath, err)
	}
	abs = filepath.Clean(abs)
	parent := filepath.Dir(abs)
	info, err := os.Lstat(parent)
	if err != nil {
		return "", fmt.Errorf("%w: database directory: %v", ErrUnsafePath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("%w: database parent is not a real directory", ErrUnsafePath)
	}
	if runtime.GOOS != "windows" {
		if info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o700 != 0o700 {
			return "", fmt.Errorf("%w: database directory permissions %04o must be private and writable", ErrUnsafePath, info.Mode().Perm())
		}
		if err := validateOwnership(info); err != nil {
			return "", err
		}
	}
	return abs, nil
}

func validateDBFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: database file: %v", ErrUnsafePath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: database path is not a regular file", ErrUnsafePath)
	}
	if runtime.GOOS != "windows" {
		if info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o600 != 0o600 {
			return fmt.Errorf("%w: database file permissions %04o must be private and writable", ErrUnsafePath, info.Mode().Perm())
		}
		if err := validateOwnership(info); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Path() string { return s.path }
