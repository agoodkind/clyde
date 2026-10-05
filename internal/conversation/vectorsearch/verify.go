package vectorsearch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/milvus-io/milvus/client/v2/column"
	"github.com/milvus-io/milvus/client/v2/entity"
	"github.com/milvus-io/milvus/client/v2/milvusclient"
	milvusstore "goodkind.io/lm-semantic-search/collection/milvus"
)

const (
	// selfMatchScore is the minimum first-result cosine score accepted by VerifyIndex.
	selfMatchScore   = 0.999
	primaryKeySpace  = 65536
	reportedMissMax  = 10
	sampleKeyPattern = "chunk_%04x"
)

// IndexCheck reports checked and passing sample counts for the cosine score threshold.
type IndexCheck struct {
	Checked int
	Found   int
	// Misses contains source primary keys for samples below the accepted score threshold.
	Misses []string
}

// VerifyIndex searches with stored-vector samples and checks the first returned score.
// A missing collection returns ErrCollectionAbsent.
func (c *Client) VerifyIndex(ctx context.Context, collectionID string, sample int) (IndexCheck, error) {
	check := IndexCheck{Checked: 0, Found: 0, Misses: nil}
	if c == nil {
		return check, errors.New("verify conversation index: client is nil")
	}
	if sample <= 0 {
		return check, errors.New("verify conversation index: sample must be positive")
	}
	milvus, isMilvus := c.store.(*milvusBackend)
	if !isMilvus {
		return check, errors.New("verify conversation index: the configured backend is not Milvus")
	}
	collectionName := CollectionName(strings.TrimSpace(collectionID))
	exists, err := c.loadCollectionIfPresent(ctx, collectionName)
	if err != nil {
		return check, err
	}
	if !exists {
		slog.WarnContext(ctx, "conversation.vectorsearch.verify_collection_absent",
			"concern", "conversation.semantic",
			"component", "conversation",
			"collection", collectionName,
		)
		return check, fmt.Errorf("verify conversation index in %s: %w", collectionName, ErrCollectionAbsent)
	}
	seen := make(map[string]bool, sample)
	for i := range sample {
		key, vector, ok, readErr := firstRowAfter(ctx, milvus.client, collectionName, fmt.Sprintf(sampleKeyPattern, i*primaryKeySpace/sample))
		if readErr != nil {
			return check, readErr
		}
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		score, searchErr := firstDenseScore(ctx, milvus.client, collectionName, vector, c.denseSearchParams)
		if searchErr != nil {
			return check, searchErr
		}
		check.Checked++
		if score >= selfMatchScore {
			check.Found++
			continue
		}
		if len(check.Misses) < reportedMissMax {
			check.Misses = append(check.Misses, key)
		}
	}
	return check, nil
}

func firstRowAfter(ctx context.Context, client *milvusclient.Client, collectionName string, after string) (string, []float32, bool, error) {
	option := milvusclient.NewQueryOption(collectionName).
		WithFilter(fmt.Sprintf("%s > %q", milvusstore.IDField, after)).
		WithOutputFields(milvusstore.IDField, milvusstore.DenseVectorField).
		WithLimit(1)
	result, err := client.Query(ctx, option)
	if err != nil {
		return "", nil, false, failRead("read a sample row of "+collectionName, err)
	}
	if result.ResultCount == 0 {
		return "", nil, false, nil
	}
	key, err := result.GetColumn(milvusstore.IDField).GetAsString(0)
	if err != nil {
		return "", nil, false, failRead("read a sample key of "+collectionName, err)
	}
	vectors, isFloat := result.GetColumn(milvusstore.DenseVectorField).(*column.ColumnFloatVector)
	if !isFloat || vectors.Len() == 0 {
		return "", nil, false, failRead("read a sample vector of "+collectionName, errors.New("dense vector column is missing"))
	}
	return key, vectors.Data()[0], true, nil
}

func firstDenseScore(ctx context.Context, client *milvusclient.Client, collectionName string, vector []float32, searchParams map[string]string) (float32, error) {
	option := milvusclient.NewSearchOption(collectionName, 1, []entity.Vector{entity.FloatVector(vector)}).
		WithANNSField(milvusstore.DenseVectorField)
	for key, value := range searchParams {
		option = option.WithSearchParam(key, value)
	}
	resultSets, err := client.Search(ctx, option)
	if err != nil {
		return 0, failRead("search "+collectionName+" with a stored vector", err)
	}
	if len(resultSets) == 0 || len(resultSets[0].Scores) == 0 {
		return 0, failRead("search "+collectionName+" with a stored vector", errors.New("search returned no result"))
	}
	return resultSets[0].Scores[0], nil
}
