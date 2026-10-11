// Package store keeps sesh's encrypted objects. Objects have structured
// names like "v1/s/<id>"; a store never sees anything but ciphertext.
package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ErrNotFound is returned by Get when no object has the name.
var ErrNotFound = errors.New("not found")

// Store holds encrypted objects by name.
type Store interface {
	Put(ctx context.Context, name string, data []byte) error
	Get(ctx context.Context, name string) ([]byte, error)
	// List returns the names of all objects under prefix, sorted.
	List(ctx context.Context, prefix string) ([]string, error)
}

var validName = regexp.MustCompile(`^[a-z0-9]+(/[a-z0-9]+)*$`)

// CheckName reports whether name is a valid object name: lowercase letters
// and digits in slash-separated parts, at most 200 bytes.
func CheckName(name string) error {
	if len(name) > 200 || !validName.MatchString(name) {
		return fmt.Errorf("invalid object name %q", name)
	}
	return nil
}

// checkPrefix allows "" or a valid name optionally ending in "/".
func checkPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	return CheckName(strings.TrimSuffix(prefix, "/"))
}

// Open returns the store named by a URL: file:///path, s3://bucket/prefix,
// or http(s)://host/ for a sesh server (see Remote).
func Open(raw string) (Store, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("store URL: %w", err)
	}
	switch u.Scheme {
	case "file":
		if u.Path == "" {
			return nil, errors.New("file store URL needs a path, like file:///var/sesh")
		}
		return &Dir{Root: u.Path}, nil
	case "s3":
		if u.Host == "" {
			return nil, errors.New("s3 store URL needs a bucket, like s3://my-bucket/prefix")
		}
		return NewS3(u.Host, strings.Trim(u.Path, "/"))
	case "http", "https":
		return NewRemote(raw)
	default:
		return nil, fmt.Errorf("unsupported store %q: use file:///path, s3://bucket/prefix or https://host", raw)
	}
}

// Dir is a store in a local directory.
type Dir struct{ Root string }

func (d *Dir) path(name string) (string, error) {
	if err := CheckName(name); err != nil {
		return "", err
	}
	return filepath.Join(d.Root, filepath.FromSlash(name)), nil
}

// Put writes atomically: a reader sees the old object or the new one, never a partial one.
func (d *Dir) Put(_ context.Context, name string, data []byte) error {
	p, err := d.path(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

func (d *Dir) Get(_ context.Context, name string) ([]byte, error) {
	p, err := d.path(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return data, err
}

func (d *Dir) List(_ context.Context, prefix string) ([]string, error) {
	if err := checkPrefix(prefix); err != nil {
		return nil, err
	}
	var names []string
	err := filepath.WalkDir(d.Root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			return nil
		}
		rel, err := filepath.Rel(d.Root, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if strings.HasPrefix(name, prefix) && CheckName(name) == nil {
			names = append(names, name)
		}
		return nil
	})
	sort.Strings(names)
	return names, err
}
