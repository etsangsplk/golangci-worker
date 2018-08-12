package httputils

import (
	"context"
	"fmt"
	"io"

	"github.com/golangci/golangci-worker/app/analytics"
	"github.com/levigross/grequests"
)

//go:generate mockgen -package httputils -source client.go -destination client_mock.go

type Client interface {
	Get(ctx context.Context, url string) (io.ReadCloser, error)
	Put(ctx context.Context, url string, jsonObj interface{}) error
}

type GrequestsClient struct{}

func (c GrequestsClient) Get(ctx context.Context, url string) (io.ReadCloser, error) {
	resp, err := grequests.Get(url, &grequests.RequestOptions{
		Context: ctx,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to make GET http request %q: %s", url, err)
	}

	if !resp.Ok {
		if cerr := resp.Close(); cerr != nil {
			analytics.Log(ctx).Warnf("Can't close %q response: %s", url, cerr)
		}

		return nil, fmt.Errorf("got error code from %q: %d", url, resp.StatusCode)
	}

	return resp, nil
}

func (c GrequestsClient) Put(ctx context.Context, url string, jsonObj interface{}) error {
	resp, err := grequests.Put(url, &grequests.RequestOptions{
		Context: ctx,
		JSON:    jsonObj,
	})
	if err != nil {
		return fmt.Errorf("unable to make PUT http request %q: %s", url, err)
	}

	if !resp.Ok {
		if cerr := resp.Close(); cerr != nil {
			analytics.Log(ctx).Warnf("Can't close %q response: %s", url, cerr)
		}

		return fmt.Errorf("got error code from %q: %d", url, resp.StatusCode)
	}

	return nil
}
