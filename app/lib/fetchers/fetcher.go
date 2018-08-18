package fetchers

import (
	"context"

	"github.com/golangci/golangci-worker/app/lib/executors"
)

//go:generate mockgen -package fetchers -source fetcher.go -destination fetcher_mock.go

type Fetcher interface {
	Fetch(ctx context.Context, repo *Repo, exec executors.Executor) error
}
