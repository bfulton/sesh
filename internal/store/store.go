// Package store keeps encrypted session archives, one object per id.
package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ErrNotFound is returned by Get when no object exists for an id.
var ErrNotFound = errors.New("no saved session for this token")

// Store holds sealed archives by object id.
type Store interface {
	Put(ctx context.Context, id string, data []byte) error
	Get(ctx context.Context, id string) ([]byte, error)
}

var validID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Key returns the object key for an id within prefix.
func Key(prefix, id string) (string, error) {
	if !validID.MatchString(id) {
		return "", fmt.Errorf("invalid object id %q", id)
	}
	return strings.TrimSuffix(prefix, "/") + "/sesh/v1/" + id, nil
}

// Open returns the store named by a URL: file:///path or s3://bucket/prefix.
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
		return NewS3(u.Host, strings.TrimPrefix(u.Path, "/"))
	default:
		return nil, fmt.Errorf("unsupported store %q: use file:///path or s3://bucket/prefix", raw)
	}
}

// Dir is a store in a local directory.
type Dir struct{ Root string }

func (d *Dir) path(id string) (string, error) {
	k, err := Key("", id)
	if err != nil {
		return "", err
	}
	return filepath.Join(d.Root, filepath.FromSlash(strings.TrimPrefix(k, "/"))), nil
}

// Put writes atomically: a reader sees the old object or the new one, never a partial one.
func (d *Dir) Put(_ context.Context, id string, data []byte) error {
	p, err := d.path(id)
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

func (d *Dir) Get(_ context.Context, id string) ([]byte, error) {
	p, err := d.path(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return data, err
}
