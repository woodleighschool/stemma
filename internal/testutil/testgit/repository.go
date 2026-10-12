// Package testgit creates Git repositories for tests.
package testgit

import (
	"testing"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
)

// Init creates a working repository at dir with commit signing disabled.
func Init(t testing.TB, dir string) *gogit.Repository {
	t.Helper()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := repo.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Commit.GpgSign = config.OptBoolFalse
	if err := repo.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return repo
}
