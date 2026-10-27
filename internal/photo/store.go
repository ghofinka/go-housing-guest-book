package photo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound is returned when a stored photo does not exist (never saved, or already purged).
var ErrNotFound = errors.New("photo not found")

// Store keeps photo bytes under opaque keys. Local disk is the v1 implementation;
// an object store (S3 and friends) can implement the same interface later.
type Store interface {
	// Save stores data under a new unique key and returns the key.
	Save(ctx context.Context, data []byte) (key string, err error)
	// Open returns the photo, or ErrNotFound.
	Open(ctx context.Context, key string) (io.ReadSeekCloser, error)
	// Delete removes the photo. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
}

// LocalStore stores photos below a single directory. Access goes through os.Root,
// so no key can escape the directory (no "../", no symlink tricks).
type LocalStore struct {
	root *os.Root
}

// NewLocalStore opens (creating if needed) dir as the photo root.
func NewLocalStore(dir string) (*LocalStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create photo dir: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open photo dir: %w", err)
	}
	return &LocalStore{root: root}, nil
}

func (s *LocalStore) Close() error { return s.root.Close() }

// Keys look like "2026/10/<uuid>.jpg": a month folder keeps directories small and
// makes retention sweeps cheap. The key carries no personal data.
func (s *LocalStore) Save(_ context.Context, data []byte) (string, error) {
	now := time.Now().UTC()
	dir := path.Join(fmt.Sprintf("%04d", now.Year()), fmt.Sprintf("%02d", int(now.Month())))
	key := path.Join(dir, uuid.NewString()+".jpg")

	if err := s.mkdirAll(dir); err != nil {
		return "", err
	}
	// Write to a temp name and rename so a crash never leaves a half-written photo under a real key.
	tmp := key + ".tmp"
	f, err := s.root.OpenFile(filepath.FromSlash(tmp), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create photo file: %w", err)
	}
	if _, err := io.Copy(f, bytes.NewReader(data)); err != nil {
		f.Close()
		_ = s.root.Remove(filepath.FromSlash(tmp))
		return "", fmt.Errorf("write photo: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = s.root.Remove(filepath.FromSlash(tmp))
		return "", fmt.Errorf("close photo: %w", err)
	}
	if err := s.root.Rename(filepath.FromSlash(tmp), filepath.FromSlash(key)); err != nil {
		_ = s.root.Remove(filepath.FromSlash(tmp))
		return "", fmt.Errorf("finalize photo: %w", err)
	}
	return key, nil
}

func (s *LocalStore) Open(_ context.Context, key string) (io.ReadSeekCloser, error) {
	f, err := s.root.Open(filepath.FromSlash(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open photo: %w", err)
	}
	return f, nil
}

func (s *LocalStore) Delete(_ context.Context, key string) error {
	err := s.root.Remove(filepath.FromSlash(key))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("delete photo: %w", err)
	}
	return nil
}

func (s *LocalStore) mkdirAll(dir string) error {
	if err := s.root.MkdirAll(filepath.FromSlash(dir), 0o700); err != nil {
		return fmt.Errorf("create photo folder: %w", err)
	}
	return nil
}
