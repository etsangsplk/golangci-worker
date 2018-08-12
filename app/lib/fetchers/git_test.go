package fetchers

import (
	"context"
	"io/ioutil"
	"testing"

	"github.com/golangci/golangci-worker/app/lib/executors"
	"github.com/stretchr/testify/assert"
)

func TestGitOnTestRepo(t *testing.T) {
	exec, err := executors.NewTempDirShell("test.git")
	assert.NoError(t, err)
	defer exec.Clean()
	g := NewGit()

	ref := "test-branch"
	cloneURL := "git@github.com:golangci/test.git"

	err = g.Fetch(context.Background(), cloneURL, ref, exec)
	assert.NoError(t, err)

	files, err := ioutil.ReadDir(exec.WorkDir())
	assert.NoError(t, err)
	assert.Len(t, files, 3)
	assert.Equal(t, ".git", files[0].Name())
	assert.Equal(t, "README.md", files[1].Name())
	assert.Equal(t, "main.go", files[2].Name())
}
