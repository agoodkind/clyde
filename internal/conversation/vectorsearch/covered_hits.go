package vectorsearch

import (
	"cmp"
	"context"
	"math"
	"slices"

	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/lm-semantic-search/collection"
)

type coverageKey struct {
	path      string
	loadRules string
}

// CoveredHits reports which local hits have equivalent embedded text.
//
// It checks stored vectors and text at each hit's byte offset.
//
// The result follows the order of the input:
//
//	covered[i] reports whether Milvus already embeds hits[i].
func (c *Client) CoveredHits(
	ctx context.Context,
	collectionID string,
	hits []semsearch.SemHit,
) ([]bool, error) {
	covered := make([]bool, len(hits))
	paths := make([]string, 0, len(hits))
	rowsByField := make(map[coverageKey][]storedFamilyRow)

	for _, hit := range hits {
		if hit.RelativePath == "" || hit.Content == "" {
			continue
		}

		paths = append(paths, hit.RelativePath)
		rowsByField[keyForHit(hit)] = nil
	}

	paths = dedupeIDs(paths)
	if len(paths) == 0 {
		return covered, nil
	}

	name := CollectionName(collectionID)
	exists, err := c.loadCollectionIfPresent(ctx, name)
	if err != nil || !exists {
		return covered, err
	}

	if err := c.readCoverageRows(ctx, name, paths, rowsByField); err != nil {
		return nil, err
	}

	for index, hit := range hits {
		covered[index] = coversHit(rowsByField[keyForHit(hit)], hit)
	}

	return covered, nil
}

func keyForHit(hit semsearch.SemHit) coverageKey {
	return coverageKey{path: hit.RelativePath, loadRules: hit.LoadRules}
}

func (c *Client) readCoverageRows(
	ctx context.Context,
	name string,
	paths []string,
	rowsByField map[coverageKey][]storedFamilyRow,
) error {
	declaration := storedRowsDeclaration()
	declaration.Scalars = append(
		declaration.Scalars,
		nullableStringColumn(loadRulesColumn, loadRulesMaxLength),
	)

	for start := 0; start < len(paths); start += conversationFilterIDBatchSize {
		end := min(start+conversationFilterIDBatchSize, len(paths))
		rows, err := c.store.QueryRows(
			ctx,
			collection.RowsRequest{
				Collection:    name,
				Declaration:   declaration,
				ItemIDs:       nil,
				PathPrefixes:  paths[start:end],
				IncludeVector: true,
			},
		)
		if err != nil {
			return failRead("read embedding coverage from "+name, err)
		}

		for _, row := range rows {
			key := coverageKey{
				path:      row.RelativePath,
				loadRules: row.Scalars[loadRulesColumn].Value.String,
			}
			if _, wanted := rowsByField[key]; !wanted || !c.usableCoverageVector(row) {
				continue
			}

			rowsByField[key] = append(rowsByField[key], familyRowFromStorage(row))
		}
	}

	return nil
}

func (c *Client) usableCoverageVector(row collection.StoredRow) bool {
	if !embeddingModelsCompatible(row.EmbeddingModel, c.embeddingModel) ||
		len(row.Vector) == 0 || (c.dimension > 0 && len(row.Vector) != c.dimension) {
		return false
	}

	nonzero := false

	for _, value := range row.Vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return false
		}

		nonzero = nonzero || value != 0
	}

	return nonzero
}

type coverageSpan struct {
	start int
	end   int
}

// coversHit compares byte ranges within one field. Both backends use the same
// field paths, but each splits the field according to its embedding limits.
func coversHit(rows []storedFamilyRow, hit semsearch.SemHit) bool {
	if hit.Content == "" {
		return false
	}

	start := splitByteOffset(hit.SplitPart)
	end := start + len(hit.Content)
	spans := make([]coverageSpan, 0, len(rows))

	for _, row := range rows {
		if !row.splitPartRecorded {
			if start == 0 && row.content == hit.Content {
				return true
			}

			continue
		}

		rowStart := splitByteOffset(row.splitPart)
		overlapStart := max(start, rowStart)
		overlapEnd := min(end, rowStart+len(row.content))
		if overlapStart >= overlapEnd ||
			row.content[overlapStart-rowStart:overlapEnd-rowStart] !=
				hit.Content[overlapStart-start:overlapEnd-start] {
			continue
		}

		spans = append(spans, coverageSpan{start: overlapStart, end: overlapEnd})
	}

	slices.SortFunc(
		spans,
		func(left coverageSpan, right coverageSpan) int {
			return cmp.Compare(left.start, right.start)
		},
	)

	coveredUntil := start

	for _, span := range spans {
		if span.start > coveredUntil {
			return false
		}

		coveredUntil = max(coveredUntil, span.end)
	}

	return coveredUntil == end
}
