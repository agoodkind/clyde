//go:build milvusintegration

package vectorsearch_test

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/milvusclient"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	milvusstore "goodkind.io/lm-semantic-search/collection/milvus"
)

const (
	sliceRejectionRepeats          = 75
	sliceRejectionText             = "harbor survey words for a rejected "
	sliceRejectionShortText        = "short message about a harbor survey"
	sliceRejectionInputBytes       = 1000
	sliceRejectionMaxSlices        = 60
	sliceRejectionCharacter        = "x"
	sliceRejectionLongBlankBytes   = 2400
	sliceRejectionColumnBlankBytes = 70000
	sliceRejectionRefusedText      = "refused short message"
	sliceRejectionKeptText         = "accepted short message"
	sliceRejectionSplitBytes       = 1100
	// sliceRejectionLeafBytes is the size of the first piece after repeated halving of a
	// message with sliceRejectionSplitBytes bytes. The piece does not end in whitespace.
	sliceRejectionLeafBytes = 68
	sliceRejectionAnySlice  = -1
	sliceCrowdQuery         = "crowdquery"
	// sliceCrowdBlankBytes produces more whitespace-only rows than sliceCrowdRankingDepth.
	sliceCrowdBlankBytes = 800000
	sliceCrowdTextRows   = 12
	// sliceCrowdDistinctKeys exceeds the total row count in eight rankings with
	// integrationSearchLimit rows each.
	sliceCrowdDistinctKeys = 100
	// sliceCrowdRankingDepth is the ranking depth of a search with integrationSearchLimit.
	sliceCrowdRankingDepth     = 64
	sliceRejectionEdgeBytes    = 600
	sliceRejectionBlankBytes   = 1800
	sliceRejectionErrorFormat  = `{"error":{"code":"context_length_exceeded","type":"invalid_request_error","message":"This model's maximum context length is 100 tokens, however the input at index %d resolved to 2000 tokens. Reduce the input length."}}`
	sliceRejectionRequestField = "input"
)

// sliceRejectionEndpoint rejects a request when an input exceeds
// sliceRejectionInputBytes or starts with the nonempty refused prefix. It records
// the rejected input and returns a vector for each input in an accepted request.
type sliceRejectionEndpoint struct {
	mutex    sync.Mutex
	rejected []string
	refused  string
	// blankQuery is the substring that vectorFor checks to select the whitespace vector.
	blankQuery string
}

func (endpoint *sliceRejectionEndpoint) acceptAll() {
	endpoint.mutex.Lock()
	defer endpoint.mutex.Unlock()
	endpoint.refused = ""
}

func (endpoint *sliceRejectionEndpoint) refuses(input string) bool {
	endpoint.mutex.Lock()
	defer endpoint.mutex.Unlock()
	return len(input) > sliceRejectionInputBytes || (endpoint.refused != "" && strings.HasPrefix(input, endpoint.refused))
}

func (endpoint *sliceRejectionEndpoint) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var body map[string]json.RawMessage
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	var inputs []string
	if err := json.Unmarshal(body[sliceRejectionRequestField], &inputs); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	for position, input := range inputs {
		if endpoint.refuses(input) {
			endpoint.mutex.Lock()
			endpoint.rejected = append(endpoint.rejected, input)
			endpoint.mutex.Unlock()
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(writer, sliceRejectionErrorFormat, position)
			return
		}
	}
	data := make([]map[string]any, 0, len(inputs))
	for position, input := range inputs {
		data = append(data, map[string]any{"embedding": endpoint.vectorFor(input), "index": position})
	}
	_ = json.NewEncoder(writer).Encode(map[string]any{"data": data})
}

// vectorFor returns a vector with one component equal to one and the others equal to zero.
// It hashes input to select that component. When input contains a nonempty blankQuery,
// it hashes WhitespaceEmbeddingInput instead.
func (endpoint *sliceRejectionEndpoint) vectorFor(input string) []float64 {
	if endpoint.blankQuery != "" && strings.Contains(input, endpoint.blankQuery) {
		input = vectorsearch.WhitespaceEmbeddingInput
	}
	digest := fnv.New32a()
	_, _ = digest.Write([]byte(input))
	vector := make([]float64, integrationDimension)
	vector[int(digest.Sum32())%integrationDimension] = 1
	return vector
}

func (endpoint *sliceRejectionEndpoint) rejections() []string {
	endpoint.mutex.Lock()
	defer endpoint.mutex.Unlock()
	return slices.Clone(endpoint.rejected)
}

// TestSliceDoesNotReuseRejectedRow checks that slices store every byte from two
// identical messages and a short message after the endpoint rejects oversized
// inputs. Each slice has a row limit of one.
func TestSliceDoesNotReuseRejectedRow(t *testing.T) {
	testCases := []struct {
		name           string
		longText       string
		wantRows       int
		wantRejections int
	}{
		{name: "whitespace at the end", longText: strings.Repeat(sliceRejectionText, sliceRejectionRepeats), wantRows: 11, wantRejections: 6},
		{
			name:           "whitespace in the middle",
			longText:       strings.Repeat("w", sliceRejectionEdgeBytes) + strings.Repeat(" ", sliceRejectionBlankBytes) + strings.Repeat("w", sliceRejectionEdgeBytes),
			wantRows:       9,
			wantRejections: 6,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			runSliceRejectionCase(t, testCase.longText, testCase.wantRows, testCase.wantRejections)
		})
	}
}

// TestSliceKeepsWholeTextAndWritesWhitespaceEdit checks that stored pieces rebuild
// the original message and search returns every piece with nonblank text. After
// replacing a trailing space with a tab, it checks that stored pieces can rebuild
// both versions of the message.
func TestSliceKeepsWholeTextAndWritesWhitespaceEdit(t *testing.T) {
	fixture := newSplitRecoveryFixture(t)
	ctx := context.Background()
	endpoint := &sliceRejectionEndpoint{mutex: sync.Mutex{}, rejected: nil, refused: "", blankQuery: ""}
	client := openSliceRejectionClient(t, fixture, endpoint)
	longText := strings.Repeat(sliceRejectionText, sliceRejectionRepeats)
	runSlicesToFinish(t, client, []semsearch.SemDoc{splitRecoveryDocument(0, longText)}, fixture.manifest)

	pieces := storedPiecesByOffset(t, fixture)
	if rebuilt := joinContiguousPieces(t, pieces); rebuilt != longText {
		t.Errorf("The stored pieces rebuild %q instead of the message %q.", rebuilt, longText)
	}
	time.Sleep(visibilityWait)
	hits, err := client.SearchConversations(ctx, splitRecoveryCollectionID, "harbor survey", integrationSearchLimit, semsearch.SearchFilter{}, 0)
	if err != nil {
		t.Fatalf("The conversation search failed: %v.", err)
	}
	hitContents := make([]string, 0, len(hits))
	for _, hit := range hits {
		hitContents = append(hitContents, hit.Content)
	}
	pieceContents := make([]string, 0, len(pieces))
	for _, piece := range pieces {
		if strings.TrimSpace(piece.content) != "" {
			pieceContents = append(pieceContents, piece.content)
		}
	}
	slices.Sort(hitContents)
	slices.Sort(pieceContents)
	if !slices.Equal(hitContents, pieceContents) {
		t.Errorf("The search results %q differ from the stored pieces with nonblank text %q.", hitContents, pieceContents)
	}

	edited := strings.TrimSuffix(longText, " ") + "\t"
	editedManifest := []semsearch.Fingerprint{{ConversationID: splitRecoveryConversationID, Value: splitRecoveryEditedPrint}}
	runSlicesToFinish(t, client, []semsearch.SemDoc{splitRecoveryDocument(0, edited)}, editedManifest)
	editedPieces := storedPiecesByOffset(t, fixture)
	if !piecesRebuild(editedPieces, edited, 0) {
		t.Errorf("The stored pieces %v do not rebuild the edited message.", editedPieces)
	}
	if !piecesRebuild(editedPieces, longText, 0) {
		t.Errorf("The stored pieces %v no longer rebuild the original message.", editedPieces)
	}
}

// TestSliceKeepsOneCharacterBesideLongWhitespace rejects a message that has one nonblank
// character and whitespace longer than the endpoint limit. The stored rows must keep the
// character, and the slices must finish.
func TestSliceKeepsOneCharacterBesideLongWhitespace(t *testing.T) {
	testCases := []struct {
		name string
		text string
	}{
		{name: "character first", text: sliceRejectionCharacter + strings.Repeat(" ", sliceRejectionLongBlankBytes)},
		{name: "character last", text: strings.Repeat(" ", sliceRejectionLongBlankBytes) + sliceRejectionCharacter},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newSplitRecoveryFixture(t)
			endpoint := &sliceRejectionEndpoint{mutex: sync.Mutex{}, rejected: nil, refused: "", blankQuery: ""}
			client := openSliceRejectionClient(t, fixture, endpoint)
			runSlicesToFinish(t, client, []semsearch.SemDoc{splitRecoveryDocument(0, testCase.text)}, fixture.manifest)
			pieces := storedPiecesByOffset(t, fixture)
			rebuilt := joinContiguousPieces(t, pieces)
			if rebuilt != testCase.text {
				t.Errorf("The stored pieces rebuild a different message with %d bytes and %d copies of the character. The expected message has %d bytes and one copy.", len(rebuilt), strings.Count(rebuilt, sliceRejectionCharacter), len(testCase.text))
			}
		})
	}
}

// TestSliceStoresWhitespaceOnlyDocumentChunks checks that stored pieces rebuild a
// message with whitespace-only document chunks. It checks that whitespace-only
// rows share one vector and that another upsert with a changed fingerprint
// preserves every row and vector.
func TestSliceStoresWhitespaceOnlyDocumentChunks(t *testing.T) {
	fixture := newSplitRecoveryFixture(t)
	ctx := context.Background()
	endpoint := &sliceRejectionEndpoint{mutex: sync.Mutex{}, rejected: nil, refused: "", blankQuery: ""}
	client := openSliceRejectionClient(t, fixture, endpoint)
	text := sliceRejectionCharacter + strings.Repeat(" ", sliceRejectionColumnBlankBytes)
	docs := []semsearch.SemDoc{splitRecoveryDocument(0, text)}
	runSlicesToFinish(t, client, docs, fixture.manifest)
	if rebuilt := joinContiguousPieces(t, storedPiecesByOffset(t, fixture)); rebuilt != text {
		t.Errorf("The stored pieces rebuild a different message with %d bytes. The expected message has %d bytes.", len(rebuilt), len(text))
	}
	stored := fixture.rows()
	blankKeys := make(map[string]struct{})
	for _, row := range stored {
		if strings.TrimSpace(row.content) == "" {
			blankKeys[row.vectorKey] = struct{}{}
		}
	}
	if len(blankKeys) != 1 {
		t.Errorf("The whitespace-only rows use %d vectors instead of 1.", len(blankKeys))
	}
	needed, err := client.SyncConversationManifest(ctx, splitRecoveryCollectionID, fixture.manifest)
	if err != nil {
		t.Fatalf("SyncConversationManifest failed: %v.", err)
	}
	if len(needed) != 0 {
		t.Fatalf("The needed list still contains %v after the slices finished.", needed)
	}

	storedBits := fixture.vectorBits(stored)
	editedManifest := []semsearch.Fingerprint{{ConversationID: splitRecoveryConversationID, Value: splitRecoveryEditedPrint}}
	runSlicesToFinish(t, client, docs, editedManifest)
	if after := fixture.requireKept(stored, storedBits); len(after) != len(stored) {
		t.Fatalf("The repeated upsert stored %d rows instead of %d.", len(after), len(stored))
	}
}

// TestSearchReturnsTextWhenBlankRowsAreNearest stores more whitespace-only rows than the
// ranking depth and sends a query with their vector in each Milvus layout. BM25 does not
// match the query. The search must return a full page of rows with text and must not
// return a blank row.
func TestSearchReturnsTextWhenBlankRowsAreNearest(t *testing.T) {
	for _, layout := range []vectorsearch.Layout{vectorsearch.LayoutShared, vectorsearch.LayoutSingle} {
		t.Run(string(layout), func(t *testing.T) {
			fixture := newSplitRecoveryFixture(t)
			ctx := context.Background()
			endpoint := &sliceRejectionEndpoint{mutex: sync.Mutex{}, rejected: nil, refused: "", blankQuery: sliceCrowdQuery}
			client := openSliceRejectionClientFor(t, fixture, endpoint, layout)
			docs := []semsearch.SemDoc{splitRecoveryDocument(0, sliceRejectionCharacter+strings.Repeat(" ", sliceCrowdBlankBytes))}
			for position := 1; position <= sliceCrowdTextRows; position++ {
				docs = append(docs, splitRecoveryDocument(position, fmt.Sprintf("%s %d", sliceRejectionKeptText, position)))
			}
			if _, err := client.UpsertConversationDocuments(ctx, splitRecoveryCollectionID, docs, fixture.manifest); err != nil {
				t.Fatalf("UpsertConversationDocuments failed: %v.", err)
			}
			time.Sleep(visibilityWait)
			hits, err := client.SearchConversations(ctx, splitRecoveryCollectionID, sliceCrowdQuery, integrationSearchLimit, semsearch.SearchFilter{}, 0)
			if err != nil {
				t.Fatalf("The conversation search failed: %v.", err)
			}
			blankRows := 0
			for _, piece := range storedPiecesByOffset(t, fixture) {
				if strings.TrimSpace(piece.content) == "" {
					blankRows++
				}
			}
			if blankRows <= sliceCrowdRankingDepth {
				t.Fatalf("The collection contains %d blank rows instead of more than %d.", blankRows, sliceCrowdRankingDepth)
			}
			textHits := 0
			for _, hit := range hits {
				if strings.TrimSpace(hit.Content) == "" {
					t.Errorf("The search returned a blank row with score %f.", hit.Score)
					continue
				}
				textHits++
			}
			if textHits != integrationSearchLimit {
				t.Errorf("The search returned %d rows with text instead of %d.", textHits, integrationSearchLimit)
			}
		})
	}
}

// TestSliceWithholdsCheckpointUntilTextIsStored checks that UpsertConversationSlice
// returns ErrInputNotStored and leaves the conversation in the needed list after
// the endpoint rejects a short message. After the endpoint accepts the text, the
// test checks that a retry stores the text and records the checkpoint.
func TestSliceWithholdsCheckpointUntilTextIsStored(t *testing.T) {
	fixture := newSplitRecoveryFixture(t)
	ctx := context.Background()
	endpoint := &sliceRejectionEndpoint{mutex: sync.Mutex{}, rejected: nil, refused: sliceRejectionRefusedText, blankQuery: ""}
	client := openSliceRejectionClient(t, fixture, endpoint)
	docs := []semsearch.SemDoc{splitRecoveryDocument(0, sliceRejectionRefusedText)}
	if _, err := client.UpsertConversationSlice(ctx, splitRecoveryCollectionID, docs, fixture.manifest); !errors.Is(err, vectorsearch.ErrInputNotStored) {
		t.Fatalf("UpsertConversationSlice returned %v instead of %v.", err, vectorsearch.ErrInputNotStored)
	}
	needed, err := client.SyncConversationManifest(ctx, splitRecoveryCollectionID, fixture.manifest)
	if err != nil {
		t.Fatalf("SyncConversationManifest failed: %v.", err)
	}
	if !slices.Equal(needed, []string{splitRecoveryConversationID}) {
		t.Fatalf("SyncConversationManifest returned %v instead of a list containing only the conversation with unstored text.", needed)
	}

	endpoint.acceptAll()
	runSlicesToFinish(t, client, docs, fixture.manifest)
	if rebuilt := joinContiguousPieces(t, storedPiecesByOffset(t, fixture)); rebuilt != sliceRejectionRefusedText {
		t.Errorf("The stored pieces rebuild %q instead of %q.", rebuilt, sliceRejectionRefusedText)
	}
	needed, err = client.SyncConversationManifest(ctx, splitRecoveryCollectionID, fixture.manifest)
	if err != nil {
		t.Fatalf("SyncConversationManifest failed: %v.", err)
	}
	if len(needed) != 0 {
		t.Fatalf("The needed list still contains %v after the retry.", needed)
	}
}

// TestSliceReportsRejectedTextImmediately checks that the slice with rejected text
// returns ErrInputNotStored and leaves the conversation in the needed list, even
// when other planned rows remain. After the endpoint accepts the text, a retry
// must store every byte.
func TestSliceReportsRejectedTextImmediately(t *testing.T) {
	var longMessage strings.Builder
	for position := 0; longMessage.Len() < sliceRejectionSplitBytes; position++ {
		fmt.Fprintf(&longMessage, "word %04d ", position)
	}
	testCases := []struct {
		name      string
		texts     []string
		refused   string
		wantSlice int
	}{
		{
			name:      "the first slice rejects the text",
			texts:     []string{sliceRejectionRefusedText, sliceRejectionKeptText + " one", sliceRejectionKeptText + " two"},
			refused:   sliceRejectionRefusedText,
			wantSlice: 0,
		},
		{
			name:      "a resumed slice rejects the text",
			texts:     []string{sliceRejectionKeptText + " one", sliceRejectionRefusedText, sliceRejectionKeptText + " two"},
			refused:   sliceRejectionRefusedText,
			wantSlice: 1,
		},
		{
			name:      "a resumed slice rejects a split piece",
			texts:     []string{longMessage.String()},
			refused:   longMessage.String()[:sliceRejectionLeafBytes],
			wantSlice: sliceRejectionAnySlice,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newSplitRecoveryFixture(t)
			ctx := context.Background()
			endpoint := &sliceRejectionEndpoint{mutex: sync.Mutex{}, rejected: nil, refused: testCase.refused, blankQuery: ""}
			client := openSliceRejectionClient(t, fixture, endpoint)
			docs := make([]semsearch.SemDoc, 0, len(testCase.texts))
			wantBytes := 0
			for position, text := range testCase.texts {
				docs = append(docs, splitRecoveryDocument(position, text))
				wantBytes += len(text)
			}

			failedSlice := runSlicesToRejectedText(t, client, docs, fixture.manifest)
			if testCase.wantSlice != sliceRejectionAnySlice && failedSlice != testCase.wantSlice {
				t.Fatalf("Slice %d reported the rejected text instead of slice %d.", failedSlice, testCase.wantSlice)
			}
			needed, err := client.SyncConversationManifest(ctx, splitRecoveryCollectionID, fixture.manifest)
			if err != nil {
				t.Fatalf("SyncConversationManifest failed: %v.", err)
			}
			if !slices.Equal(needed, []string{splitRecoveryConversationID}) {
				t.Fatalf("SyncConversationManifest returned %v instead of a list containing only the conversation with unstored text.", needed)
			}

			endpoint.acceptAll()
			runSlicesToFinish(t, client, docs, fixture.manifest)
			if rebuilt := joinContiguousPieces(t, storedPiecesByOffset(t, fixture)); rebuilt != strings.Join(testCase.texts, "") {
				t.Errorf("The stored pieces rebuild %d bytes that differ from the messages' %d bytes.", len(rebuilt), wantBytes)
			}
			needed, err = client.SyncConversationManifest(ctx, splitRecoveryCollectionID, fixture.manifest)
			if err != nil {
				t.Fatalf("SyncConversationManifest failed: %v.", err)
			}
			if len(needed) != 0 {
				t.Fatalf("The needed list still contains %v after the retry.", needed)
			}
		})
	}
}

// runSlicesToRejectedText runs slices until one reports ErrInputNotStored and returns
// that slice's position.
func runSlicesToRejectedText(t *testing.T, client *vectorsearch.Client, docs []semsearch.SemDoc, manifest []semsearch.Fingerprint) int {
	t.Helper()
	for slice := range sliceRejectionMaxSlices {
		_, upsertErr := client.UpsertConversationSlice(context.Background(), splitRecoveryCollectionID, docs, manifest)
		if errors.Is(upsertErr, vectorsearch.ErrInputNotStored) {
			return slice
		}
		var partial *vectorsearch.PartialUpsertError
		if !errors.As(upsertErr, &partial) {
			t.Fatalf("Slice %d returned %v instead of a partial upsert error.", slice, upsertErr)
		}
	}
	t.Fatalf("The slices did not report the rejected text within %d slices.", sliceRejectionMaxSlices)
	return sliceRejectionAnySlice
}

// TestSearchReturnsTextWhenBlankRowsUseManyVectors stores whitespace-only rows with
// vectors based on the query vector. It checks that search returns a full page of
// rows with nonblank text in each Milvus layout.
func TestSearchReturnsTextWhenBlankRowsUseManyVectors(t *testing.T) {
	for _, layout := range []vectorsearch.Layout{vectorsearch.LayoutShared, vectorsearch.LayoutSingle} {
		t.Run(string(layout), func(t *testing.T) {
			fixture := newSplitRecoveryFixture(t)
			ctx := context.Background()
			endpoint := &sliceRejectionEndpoint{mutex: sync.Mutex{}, rejected: nil, refused: "", blankQuery: sliceCrowdQuery}
			client := openSliceRejectionClientFor(t, fixture, endpoint, layout)
			docs := make([]semsearch.SemDoc, 0, sliceCrowdTextRows)
			for position := 1; position <= sliceCrowdTextRows; position++ {
				docs = append(docs, splitRecoveryDocument(position, fmt.Sprintf("%s %d", sliceRejectionKeptText, position)))
			}
			if _, err := client.UpsertConversationDocuments(ctx, splitRecoveryCollectionID, docs, fixture.manifest); err != nil {
				t.Fatalf("UpsertConversationDocuments failed: %v.", err)
			}
			if err := vectorsearch.WriteDistinctBlankRowsForTest(ctx, client, splitRecoveryCollectionID, splitRecoveryConversationID, sliceCrowdQuery, sliceCrowdDistinctKeys); err != nil {
				t.Fatalf("WriteDistinctBlankRowsForTest failed: %v.", err)
			}
			time.Sleep(visibilityWait)
			hits, err := client.SearchConversations(ctx, splitRecoveryCollectionID, sliceCrowdQuery, integrationSearchLimit, semsearch.SearchFilter{}, 0)
			if err != nil {
				t.Fatalf("The conversation search failed: %v.", err)
			}
			textHits := 0
			for _, hit := range hits {
				if strings.TrimSpace(hit.Content) == "" {
					t.Errorf("The search returned a blank row with score %f.", hit.Score)
					continue
				}
				textHits++
			}
			if textHits != integrationSearchLimit {
				t.Errorf("The search returned %d rows with text instead of %d.", textHits, integrationSearchLimit)
			}
		})
	}
}

// storedPiece stores one row's path, its byte offset in that path's document chunk, and
// its content.
type storedPiece struct {
	path    string
	offset  int
	content string
}

// storedPiecesByOffset reads the conversation's rows and sorts them by path and byte offset.
func storedPiecesByOffset(t *testing.T, fixture *splitRecoveryFixture) []storedPiece {
	t.Helper()
	result, err := fixture.reader.Query(context.Background(), milvusclient.NewQueryOption(fixture.collectionName).
		WithFilter(fmt.Sprintf("%s == %q", splitRecoveryConversation, splitRecoveryConversationID)).
		WithOutputFields(milvusstore.RelativePathField, milvusstore.ContentField, milvusstore.SplitPartField).
		WithConsistencyLevel(entity.ClStrong))
	if err != nil {
		t.Fatalf("Query failed: %v.", err)
	}
	pieces := make([]storedPiece, 0, result.ResultCount)
	for position := range result.ResultCount {
		path, pathErr := result.GetColumn(milvusstore.RelativePathField).GetAsString(position)
		content, contentErr := result.GetColumn(milvusstore.ContentField).GetAsString(position)
		splitPart, splitErr := result.GetColumn(milvusstore.SplitPartField).GetAsInt64(position)
		if readErr := errors.Join(pathErr, contentErr, splitErr); readErr != nil {
			t.Fatalf("The test could not read stored row %d: %v.", position, readErr)
		}
		pieces = append(pieces, storedPiece{path: path, offset: max(int(splitPart)-1, 0), content: content})
	}
	slices.SortFunc(pieces, func(left storedPiece, right storedPiece) int {
		return cmp.Or(cmp.Compare(left.path, right.path), cmp.Compare(left.offset, right.offset))
	})
	return pieces
}

// joinContiguousPieces joins pieces sorted by path and offset. It fails on a gap or an
// overlap inside one path.
func joinContiguousPieces(t *testing.T, pieces []storedPiece) string {
	t.Helper()
	var joined strings.Builder
	path := ""
	pathBytes := 0
	for _, piece := range pieces {
		if piece.path != path {
			path = piece.path
			pathBytes = 0
		}
		if piece.offset != pathBytes {
			t.Fatalf("A stored piece of %s starts at byte %d instead of byte %d.", piece.path, piece.offset, pathBytes)
		}
		joined.WriteString(piece.content)
		pathBytes += len(piece.content)
	}
	return joined.String()
}

// piecesRebuild reports whether pieces at contiguous offsets rebuild want from offset.
// After an edit, one offset can have an earlier piece and an edited piece.
func piecesRebuild(pieces []storedPiece, want string, offset int) bool {
	if offset == len(want) {
		return true
	}
	for _, piece := range pieces {
		if piece.offset != offset || piece.content == "" || !strings.HasPrefix(want[offset:], piece.content) {
			continue
		}
		if piecesRebuild(pieces, want, offset+len(piece.content)) {
			return true
		}
	}
	return false
}

func openSliceRejectionClient(t *testing.T, fixture *splitRecoveryFixture, endpoint *sliceRejectionEndpoint) *vectorsearch.Client {
	t.Helper()
	return openSliceRejectionClientFor(t, fixture, endpoint, vectorsearch.LayoutShared)
}

func openSliceRejectionClientFor(t *testing.T, fixture *splitRecoveryFixture, endpoint *sliceRejectionEndpoint, layout vectorsearch.Layout) *vectorsearch.Client {
	t.Helper()
	ctx := context.Background()
	server := httptest.NewServer(endpoint)
	t.Cleanup(server.Close)
	var client *vectorsearch.Client
	var err error
	deadline := time.Now().Add(time.Minute)
	for {
		client, err = vectorsearch.Open(ctx, vectorsearch.Options{
			Backend:            vectorsearch.BackendMilvus,
			MilvusAddress:      fixture.address,
			MilvusDatabase:     "default",
			MilvusLayout:       layout,
			EmbeddingBaseURL:   server.URL + "/v1",
			EmbeddingModel:     integrationModel,
			EmbeddingDimension: integrationDimension,
			EmbeddingAPIKey:    integrationEmbeddingKey,
			EmbeddingTimeout:   time.Minute,
			CheckpointDir:      fixture.checkpointDir,
			IngestSliceRows:    1,
		})
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		t.Fatalf("vectorsearch.Open failed: %v.", err)
	}
	t.Cleanup(func() { _ = client.Close(ctx) })
	return client
}

func runSlicesToFinish(t *testing.T, client *vectorsearch.Client, docs []semsearch.SemDoc, manifest []semsearch.Fingerprint) {
	t.Helper()
	for slice := range sliceRejectionMaxSlices {
		_, upsertErr := client.UpsertConversationSlice(context.Background(), splitRecoveryCollectionID, docs, manifest)
		if upsertErr == nil {
			return
		}
		var partial *vectorsearch.PartialUpsertError
		if !errors.As(upsertErr, &partial) {
			t.Fatalf("Slice %d returned %v instead of a partial upsert error.", slice, upsertErr)
		}
	}
	t.Fatalf("UpsertConversationSlice did not complete within %d slices.", sliceRejectionMaxSlices)
}

func runSliceRejectionCase(t *testing.T, longText string, wantRows int, wantRejections int) {
	t.Helper()
	fixture := newSplitRecoveryFixture(t)
	ctx := context.Background()
	endpoint := &sliceRejectionEndpoint{mutex: sync.Mutex{}, rejected: nil, refused: "", blankQuery: ""}
	client := openSliceRejectionClient(t, fixture, endpoint)
	docs := []semsearch.SemDoc{
		splitRecoveryDocument(0, longText),
		splitRecoveryDocument(1, longText),
		splitRecoveryDocument(2, sliceRejectionShortText),
	}
	runSlicesToFinish(t, client, docs, fixture.manifest)

	rejections := endpoint.rejections()
	if len(rejections) != wantRejections {
		t.Fatalf("The endpoint rejected %d inputs instead of the expected %d.", len(rejections), wantRejections)
	}
	contents := make([]string, 0)
	storedBytes := 0
	for _, row := range fixture.rows() {
		contents = append(contents, row.content)
		storedBytes += len(row.content)
	}
	if slices.ContainsFunc(contents, func(content string) bool { return len(content) > sliceRejectionInputBytes }) {
		t.Fatal("The collection contains an input that exceeds the endpoint's byte limit.")
	}
	if wantBytes := 2*len(longText) + len(sliceRejectionShortText); storedBytes != wantBytes {
		t.Fatalf("The stored rows contain %d bytes instead of the messages' %d bytes.", storedBytes, wantBytes)
	}
	if len(contents) != wantRows {
		t.Fatalf("The collection contains %d rows instead of the expected %d.", len(contents), wantRows)
	}
	needed, err := client.SyncConversationManifest(ctx, splitRecoveryCollectionID, fixture.manifest)
	if err != nil {
		t.Fatalf("SyncConversationManifest failed: %v.", err)
	}
	if len(needed) != 0 {
		t.Fatalf("The needed list still contains %v after the slices finished.", needed)
	}
}
