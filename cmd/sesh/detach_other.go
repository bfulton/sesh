//go:build !unix

package main

import "errors"

func detach(args []string, env []string) error { return errors.New("not supported") }

func lockFile(path string) (func(), error) { return func() {}, nil }
