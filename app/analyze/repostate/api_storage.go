//nolint:dupl
package repostate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/golangci/golangci-worker/app/lib/httputils"
)

type APIStorage struct {
	host   string
	client httputils.Client
}

func NewAPIStorage(client httputils.Client) *APIStorage {
	return &APIStorage{
		client: client,
		host:   os.Getenv("API_URL"),
	}
}

func (s APIStorage) getAnalysisURL(owner, name, analysisID string) string {
	return fmt.Sprintf("%s/v1/repos/github.com/%s/%s/repoanalyzes/%s", s.host, owner, name, analysisID)
}

func (s APIStorage) UpdateState(ctx context.Context, owner, name, analysisID string, state *State) error {
	return s.client.Put(ctx, s.getAnalysisURL(owner, name, analysisID), state)
}

func (s APIStorage) GetState(ctx context.Context, owner, name, analysisID string) (*State, error) {
	bodyReader, err := s.client.Get(ctx, s.getAnalysisURL(owner, name, analysisID))
	if err != nil {
		return nil, err
	}

	defer bodyReader.Close()

	var state State
	if err = json.NewDecoder(bodyReader).Decode(&state); err != nil {
		return nil, fmt.Errorf("can't read json body: %s", err)
	}

	return &state, nil
}
