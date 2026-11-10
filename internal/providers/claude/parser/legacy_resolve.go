package parser

import (
	"crypto/sha256"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"goodkind.io/clyde/internal/adapter/anthropic"
	"goodkind.io/clyde/internal/reorienttag"
)

const (
	// Resolution permits at most this many nested prior summaries.
	legacyMaximumDepth = 64
	// The resolver uses this position for text that does not come from a request
	// message, such as prior summary prose.
	legacyNoPosition = -1
)

// Unframed transcript headings use these role names.
var legacyHeadings = [...]string{"User", "Assistant", "System", "Other"}

// legacyPiece records one typed segment of a stored wrapper body in summary
// content offsets. When prior has a value, the segment text equals that prior
// summary's content from priorBase, and the resolver reads its kinds from that
// summary.
type legacyPiece struct {
	start       int
	end         int
	role        reorienttag.SegmentRole
	kind        reorienttag.SegmentKind
	position    int
	prior       string
	priorBase   int
	sourceStart int
	sourceEnd   int
}

// legacyDecomposition stores a wrapper body that equals one replayed request's
// rendering byte for byte. It includes typed pieces, a map from stored byte
// offsets to source byte offsets, and request statistics.
type legacyDecomposition struct {
	content     string
	requestID   string
	bodyStart   int
	bodyEnd     int
	persisted   persistedRendering
	pieces      []legacyPiece
	positions   int
	blobSources map[string]resolvedSource
}

// resolvedPiece records one typed range of summary content and the summary and
// request message position it comes from.
type resolvedPiece struct {
	start    int
	end      int
	role     reorienttag.SegmentRole
	kind     reorienttag.SegmentKind
	summary  string
	position int
}

// summaryContentLine stores the string content of a compact summary message.
type summaryContentLine struct {
	Content string `json:"content"`
}

// legacyResolver reconstructs unframed transcript wrappers in one transcript
// and caches each summary result.
type legacyResolver struct {
	reader         *provenanceReader
	contents       map[string]string
	digests        map[[sha256.Size]byte]string
	decompositions map[string]*legacyDecomposition
	failures       map[string]error
}

// legacyResolution stores one summary's typed span and the statistics the
// parser logs for it.
type legacyResolution struct {
	span     reorienttag.TranscriptSpan
	report   legacyResolutionReport
	segments int
}

// legacyResolutionReport stores counts of summaries, request positions,
// distinct direct blobs, blobs with string content, and distinct retained
// request positions across every summary a resolution reads.
type legacyResolutionReport struct {
	requestID         string
	summaries         int
	positions         int
	directBlobs       int
	normalizedBlobs   int
	retainedPositions int
}

func newLegacyResolver(reader *provenanceReader) *legacyResolver {
	return &legacyResolver{
		reader:         reader,
		contents:       make(map[string]string),
		digests:        nil,
		decompositions: make(map[string]*legacyDecomposition),
		failures:       make(map[string]error),
	}
}

// summaryContent returns the stored content string of a compact summary
// record.
func (resolver *legacyResolver) summaryContent(uuid string) (string, error) {
	if content, ok := resolver.contents[uuid]; ok {
		return content, nil
	}
	summary, ok := resolver.reader.index.summaries[uuid]
	if !ok {
		return "", newResolutionError("compact summary "+uuid, "the transcript has no such summary record")
	}
	message, err := resolver.reader.lineMessage(summary.span)
	if err != nil {
		return "", err
	}
	var decoded summaryContentLine
	if err := json.Unmarshal(message, &decoded); err != nil {
		return "", newResolutionError("compact summary "+uuid, "decode content failed", err)
	}
	resolver.contents[uuid] = decoded.Content
	return decoded.Content, nil
}

// summaryForText returns the compact summary in the transcript with exactly
// this content, or an empty string.
func (resolver *legacyResolver) summaryForText(text string) string {
	if resolver.digests == nil {
		resolver.digests = make(map[[sha256.Size]byte]string)
		for _, uuid := range resolver.reader.index.order {
			if _, ok := resolver.reader.index.summaries[uuid]; !ok {
				continue
			}
			content, err := resolver.summaryContent(uuid)
			if err != nil {
				continue
			}
			digest := sha256.Sum256([]byte(content))
			if _, seen := resolver.digests[digest]; !seen {
				resolver.digests[digest] = uuid
			}
		}
	}
	return resolver.digests[sha256.Sum256([]byte(text))]
}

// resolveSummary returns the typed span of a summary's outer wrapper, starting
// at the open tag and ending after the close tag that full reconstruction
// matched. It omits segments containing only whitespace.
func (resolver *legacyResolver) resolveSummary(uuid string) (legacyResolution, error) {
	noResolution := legacyResolution{
		span:     reorienttag.TranscriptSpan{Start: 0, End: 0, Segments: nil},
		report:   legacyResolutionReport{requestID: "", summaries: 0, positions: 0, directBlobs: 0, normalizedBlobs: 0, retainedPositions: 0},
		segments: 0,
	}
	decomposition, err := resolver.decompose(uuid)
	if err != nil {
		return noResolution, err
	}
	visited := make(map[string]bool)
	pieces, err := resolver.resolveRange(uuid, decomposition.bodyStart, false, visited, 0)
	if err != nil {
		return noResolution, err
	}
	segments := make([]reorienttag.Segment, 0, len(pieces))
	retained := make(map[string]bool)
	for _, piece := range pieces {
		if piece.position != legacyNoPosition {
			retained[piece.summary+":"+strconv.Itoa(piece.position)] = true
		}
		text := decomposition.content[piece.start:piece.end]
		if strings.TrimSpace(text) == "" {
			continue
		}
		segments = append(segments, reorienttag.Segment{Role: piece.role, Kind: piece.kind, Text: text})
	}
	report := resolver.report(visited)
	report.requestID = decomposition.requestID
	report.retainedPositions = len(retained)
	return legacyResolution{
		span: reorienttag.TranscriptSpan{
			Start:    decomposition.bodyStart - len(reorienttag.PreCompactionTranscriptOpen) - 1,
			End:      decomposition.bodyEnd + 1 + len(reorienttag.PreCompactionTranscriptClose),
			Segments: segments,
		},
		report:   report,
		segments: len(segments),
	}, nil
}

// report totals request positions and distinct direct blobs across the visited
// summaries.
func (resolver *legacyResolver) report(visited map[string]bool) legacyResolutionReport {
	report := legacyResolutionReport{requestID: "", summaries: len(visited), positions: 0, directBlobs: 0, normalizedBlobs: 0, retainedPositions: 0}
	blobs := make(map[string]resolvedSource)
	for uuid := range visited {
		decomposition := resolver.decompositions[uuid]
		report.positions += decomposition.positions
		maps.Copy(blobs, decomposition.blobSources)
	}
	report.directBlobs = len(blobs)
	for _, source := range blobs {
		if source == resolvedSourceNormalizedBlob {
			report.normalizedBlobs++
		}
	}
	return report
}

// resolveRange returns typed pieces covering summary content starting at from.
// It replaces pieces that repeat a prior summary by mapping that summary's
// pieces into this content. When outside is true, it treats content before and
// after the wrapper body as user chat text because the request stored it as
// user text.
func (resolver *legacyResolver) resolveRange(uuid string, from int, outside bool, visited map[string]bool, depth int) ([]resolvedPiece, error) {
	if depth > legacyMaximumDepth {
		return nil, newResolutionError("compact summary "+uuid, "the inherited transcript chain exceeds "+strconv.Itoa(legacyMaximumDepth)+" summaries")
	}
	decomposition, err := resolver.decompose(uuid)
	if err != nil {
		return nil, err
	}
	visited[uuid] = true
	out := make([]resolvedPiece, 0, len(decomposition.pieces)+2)
	if outside && from < decomposition.bodyStart {
		out = append(out, inheritedTextPiece(from, decomposition.bodyStart))
	}
	for _, piece := range decomposition.pieces {
		if piece.end <= from {
			continue
		}
		start := max(piece.start, from)
		if piece.prior == "" {
			out = append(out, resolvedPiece{start: start, end: piece.end, role: piece.role, kind: piece.kind, summary: uuid, position: piece.position})
			continue
		}
		inherited, err := resolver.resolveRange(piece.prior, decomposition.priorOffset(piece, start), true, visited, depth+1)
		if err != nil {
			return nil, err
		}
		for _, child := range inherited {
			childStart := max(decomposition.contentOffset(piece, child.start), start)
			childEnd := min(decomposition.contentOffset(piece, child.end), piece.end)
			if childEnd <= childStart {
				continue
			}
			child.start = childStart
			child.end = childEnd
			out = append(out, child)
		}
	}
	if outside {
		out = append(out, inheritedTextPiece(max(from, decomposition.bodyEnd), len(decomposition.content)))
	}
	return out, nil
}

// inheritedTextPiece returns user chat text for prior summary content outside
// its wrapper body.
func inheritedTextPiece(start int, end int) resolvedPiece {
	return resolvedPiece{
		start:    start,
		end:      end,
		role:     reorienttag.SegmentRoleUser,
		kind:     reorienttag.SegmentKindChat,
		summary:  "",
		position: legacyNoPosition,
	}
}

// priorOffset converts a content offset inside an inherited piece to the
// matching prior summary offset.
func (decomposition *legacyDecomposition) priorOffset(piece legacyPiece, contentOffset int) int {
	source := int(decomposition.persisted.sourceMap[contentOffset-decomposition.bodyStart])
	return piece.priorBase + source - piece.sourceStart
}

// contentOffset converts a prior summary offset to the matching content offset
// inside an inherited piece.
func (decomposition *legacyDecomposition) contentOffset(piece legacyPiece, priorOffset int) int {
	source := min(max(piece.sourceStart+priorOffset-piece.priorBase, piece.sourceStart), piece.sourceEnd)
	return decomposition.bodyStart + decomposition.persisted.outputOffset(source)
}

// decompose returns a summary's cached reconstruction or cached failure.
func (resolver *legacyResolver) decompose(uuid string) (*legacyDecomposition, error) {
	if decomposition, ok := resolver.decompositions[uuid]; ok {
		return decomposition, nil
	}
	if err, ok := resolver.failures[uuid]; ok {
		return nil, err
	}
	decomposition, err := resolver.decomposeSummary(uuid)
	if err != nil {
		resolver.failures[uuid] = err
		return nil, err
	}
	resolver.decompositions[uuid] = decomposition
	return decomposition, nil
}

func (resolver *legacyResolver) decomposeSummary(uuid string) (*legacyDecomposition, error) {
	subject := "compact summary " + uuid
	content, err := resolver.summaryContent(uuid)
	if err != nil {
		return nil, err
	}
	starts := legacyBodyStarts(content)
	if len(starts) == 0 {
		return nil, newResolutionError(subject, "the summary has no unframed transcript wrapper")
	}
	candidates := resolver.reader.index.summaries[uuid].candidates
	if len(candidates) == 0 {
		return nil, newResolutionError(subject, "the transcript has no replayed source request before the summary")
	}
	failures := make([]error, 0, len(candidates))
	for _, candidate := range candidates {
		decomposition, err := resolver.decomposeWith(uuid, content, starts, candidate)
		if err == nil {
			return decomposition, nil
		}
		failures = append(failures, err)
	}
	return nil, newResolutionError(subject, "no replayed source request reproduces the wrapper", failures...)
}

// decomposeWith resolves every reference in one request snapshot and decodes
// the request. It treats the last user message as the compaction instruction
// and requires exactly one wrapper body to equal the rendering. Any unresolved
// reference fails the snapshot.
func (resolver *legacyResolver) decomposeWith(
	uuid string,
	content string,
	starts []int,
	snapshot requestSnapshot,
) (*legacyDecomposition, error) {
	subject := "request " + snapshot.id
	messages := make([]resolvedMessage, 0, len(snapshot.refs))
	unresolved := make([]error, 0)
	for _, ref := range snapshot.refs {
		message, err := resolver.reader.resolveReference(ref)
		if err != nil {
			unresolved = append(unresolved, err)
			continue
		}
		messages = append(messages, message)
	}
	if len(unresolved) > 0 {
		return nil, newResolutionError(subject, strconv.Itoa(len(unresolved))+" unresolved references", unresolved...)
	}
	request, err := decodeSnapshotRequest(messages)
	if err != nil {
		return nil, err
	}
	instruction := lastUserMessage(request)
	if instruction < 1 {
		return nil, newResolutionError(subject, "the request has no user message after its first message")
	}
	match, ok := matchLegacyBody(request, instruction, content, starts)
	if !ok {
		return nil, newResolutionError(subject, "exactly one wrapper body must equal the replayed rendering")
	}
	decomposition := resolver.buildDecomposition(uuid, content, match)
	decomposition.requestID = snapshot.id
	decomposition.positions = len(snapshot.refs)
	for index, ref := range snapshot.refs {
		if messages[index].source != resolvedSourceGroup {
			decomposition.blobSources[ref] = messages[index].source
		}
	}
	return decomposition, nil
}

func lastUserMessage(request anthropic.CompactionRequest) int {
	for index, message := range slices.Backward(request.Messages) {
		if message.Role == anthropic.CompactionRoleUser {
			return index
		}
	}
	return -1
}

// legacyBodyStarts returns the offset after each transcript open tag line that
// a role heading follows.
func legacyBodyStarts(content string) []int {
	open := reorienttag.PreCompactionTranscriptOpen + "\n"
	starts := make([]int, 0, 1)
	position := 0
	for {
		offset := strings.Index(content[position:], open)
		if offset < 0 {
			return starts
		}
		bodyStart := position + offset + len(open)
		if legacyHeadingLength(content[bodyStart:]) > 0 {
			starts = append(starts, bodyStart)
		}
		position = bodyStart
	}
}

func legacyHeadingLength(text string) int {
	for _, heading := range legacyHeadings {
		prefix := legacyHeadingPrefix + heading + legacySegmentSeparator
		if strings.HasPrefix(text, prefix) {
			return len(prefix)
		}
	}
	return 0
}

// legacyMatch stores one wrapper body range and the rendering that equals it.
type legacyMatch struct {
	bodyStart int
	bodyEnd   int
	rendering legacyRendering
	persisted persistedRendering
}

// matchLegacyBody tries each open position with each later close tag and keeps
// bodies that equal a rendering of the request. It succeeds only with exactly
// one match. That match determines the outer close tag, which may differ from
// the first close tag.
func matchLegacyBody(
	request anthropic.CompactionRequest,
	instruction int,
	content string,
	starts []int,
) (legacyMatch, bool) {
	full := renderLegacy(request, legacyCut{message: 0, segment: 0, headRunes: 0}, instruction)
	fullPersisted := persistLegacy(full.source)
	closing := "\n" + reorienttag.PreCompactionTranscriptClose
	matches := make([]legacyMatch, 0, 1)
	for _, bodyStart := range starts {
		position := bodyStart
		for {
			offset := strings.Index(content[position:], closing)
			if offset < 0 {
				break
			}
			bodyEnd := position + offset
			if rendering, persisted, ok := matchLegacyCut(request, instruction, full, fullPersisted, content[bodyStart:bodyEnd]); ok {
				matches = append(matches, legacyMatch{bodyStart: bodyStart, bodyEnd: bodyEnd, rendering: rendering, persisted: persisted})
			}
			position = bodyEnd + 1
		}
	}
	if len(matches) != 1 {
		return legacyMatch{
			bodyStart: 0,
			bodyEnd:   0,
			rendering: legacyRendering{source: "", pieces: nil},
			persisted: persistedRendering{text: "", sourceMap: nil},
		}, false
	}
	return matches[0], true
}

// matchLegacyCut matches the body after its first heading to a suffix of the
// full rendering to find the cut segment and head length in runes. It renders
// the request from that cut and returns the result only when the stored body
// equals it byte for byte.
func matchLegacyCut(
	request anthropic.CompactionRequest,
	instruction int,
	full legacyRendering,
	fullPersisted persistedRendering,
	body string,
) (legacyRendering, persistedRendering, bool) {
	noRendering := legacyRendering{source: "", pieces: nil}
	noPersisted := persistedRendering{text: "", sourceMap: nil}
	headingLength := legacyHeadingLength(body)
	rest := body[headingLength:]
	if headingLength == 0 || rest == "" || !strings.HasSuffix(fullPersisted.text, rest) {
		return noRendering, noPersisted, false
	}
	source := int(fullPersisted.sourceMap[len(fullPersisted.text)-len(rest)])
	piece, ok := segmentPieceAt(full.pieces, source)
	if !ok {
		return noRendering, noPersisted, false
	}
	headBytes := piece.headBytes + source - piece.sourceStart
	if !isRuneBoundary(piece.fullText, headBytes) {
		return noRendering, noPersisted, false
	}
	cut := legacyCut{message: piece.message, segment: piece.segment, headRunes: utf8.RuneCountInString(piece.fullText[:headBytes])}
	rendering := renderLegacy(request, cut, instruction)
	persisted := persistLegacy(rendering.source)
	if persisted.text != body {
		return noRendering, noPersisted, false
	}
	return rendering, persisted, true
}

func segmentPieceAt(pieces []renderPiece, source int) (renderPiece, bool) {
	for _, piece := range pieces {
		if !piece.framing && piece.sourceStart <= source && source < piece.sourceEnd {
			return piece, true
		}
	}
	return framingPiece(0, ""), false
}

// buildDecomposition converts rendered segment pieces to content offsets. When
// a user chat segment equals another compact summary's text, it records that
// summary as the segment's prior.
func (resolver *legacyResolver) buildDecomposition(uuid string, content string, match legacyMatch) *legacyDecomposition {
	pieces := make([]legacyPiece, 0, len(match.rendering.pieces))
	for _, piece := range match.rendering.pieces {
		if piece.framing {
			continue
		}
		prior := ""
		if piece.role == reorienttag.SegmentRoleUser && piece.kind == reorienttag.SegmentKindChat {
			if candidate := resolver.summaryForText(piece.fullText); candidate != uuid {
				prior = candidate
			}
		}
		pieces = append(pieces, legacyPiece{
			start:       match.bodyStart + match.persisted.outputOffset(piece.sourceStart),
			end:         match.bodyStart + match.persisted.outputOffset(piece.sourceEnd),
			role:        piece.role,
			kind:        piece.kind,
			position:    piece.message,
			prior:       prior,
			priorBase:   piece.headBytes,
			sourceStart: piece.sourceStart,
			sourceEnd:   piece.sourceEnd,
		})
	}
	return &legacyDecomposition{
		content:     content,
		requestID:   "",
		bodyStart:   match.bodyStart,
		bodyEnd:     match.bodyEnd,
		persisted:   match.persisted,
		pieces:      pieces,
		positions:   0,
		blobSources: make(map[string]resolvedSource),
	}
}
