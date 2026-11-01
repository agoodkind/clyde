package vectorsearch

import (
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"unicode/utf8"

	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/transcript"
	"goodkind.io/gksyntax/shelldecomp"
	"goodkind.io/lm-semantic-search/collection"
)

const (
	toolSequenceCheckStoredUser = "stored_user_turn"
	toolSequenceCheckName       = "tool_name"
	toolSequenceCheckPosition   = "tool_position"
	toolSequenceCheckContent    = "tool_content"
	toolSequenceCheckComponents = "tool_components"
	toolProjectionGrouped       = "grouped"
	toolProjectionActual        = "actual"
	toolProjectionTranscript    = "transcript"
	toolProjectionOlderRun      = "older_run"
	// minimumOlderRunRows requires at least two rows for an older run because
	// one row cannot establish order.
	minimumOlderRunRows  = 2
	toolComponentTokens  = "tok"
	toolComponentCommand = "cmd"
	toolComponentInput   = "in"
	// historicalToolSummaryBytes is the input limit of the historical token row.
	historicalToolSummaryBytes = 2000
)

var toolComponentSegments = []string{toolComponentTokens, toolComponentCommand, toolComponentInput, "out"}

type storedUserMessage struct {
	message int
	texts   []string
}

// toolProjection associates message and tool positions with native calls under
// one message numbering.
type toolProjection struct {
	name       string
	calls      []SourceToolTime
	positions  map[[2]int]int
	users      []transcript.UserTurn
	otherUsers []transcript.UserTurn
	// known records transcript coordinates even when comparison does not prove
	// their native positions.
	known map[[2]int]bool
	// residual disables stored user checks when grouped and actual projections
	// check only base rows absent from transcript coordinates. Those user rows
	// may use another message numbering.
	residual bool
}

type toolSequenceDifference struct {
	check      string
	position   int
	storedName string
	native     SourceToolTime
	previous   SourceToolTime
}

// bindToolFamilies matches base rows through the transcript, grouped, and
// actual projections and older component rows through the actual projection.
func (lookup sourceTimeLookup) bindToolFamilies(families map[string][]collection.StoredRow) {
	base, components := toolFamilyKeys(families)
	if !lookup.toolCallsComplete {
		lookup.bindUnavailable(base, false)
		lookup.bindUnavailable(components, true)
		return
	}
	mixed := len(components) > 0
	users := lookup.storedUserMessages(families)
	grouped, actual := lookup.toolProjections(mixed)
	lookup.bindBaseFamilies(base, lookup.transcriptProjection(), []toolProjection{grouped, actual}, users, families)
	lookup.bindComponentFamilies(components, actual, users, families)
}

func (lookup sourceTimeLookup) bindUnavailable(keys []string, component bool) {
	for _, key := range keys {
		lookup.toolBindings[key] = toolBinding{call: emptySourceToolTime(), reason: SourceOutcomeIdentityUnavailable, component: component, projection: ""}
	}
}

func toolFamilyKeys(families map[string][]collection.StoredRow) ([]string, []string) {
	base := make([]string, 0)
	components := make([]string, 0)
	for key, rows := range families {
		if !strings.HasPrefix(key, conversationToolFamily+familyPartSeparator) {
			continue
		}
		isComponent := slices.ContainsFunc(rows, func(row collection.StoredRow) bool {
			return isToolComponentPath(key, row.RelativePath)
		})
		if isComponent {
			components = append(components, key)
			continue
		}
		base = append(base, key)
	}
	byFamilyOrder := func(left string, right string) int {
		return slices.Compare(familyOrder(left), familyOrder(right))
	}
	slices.SortFunc(base, byFamilyOrder)
	slices.SortFunc(components, byFamilyOrder)
	return base, components
}

func isToolComponentPath(key string, relativePath string) bool {
	remainder, found := strings.CutPrefix(relativePath, key+familyPartSeparator)
	if !found {
		return false
	}
	segment, _, _ := strings.Cut(remainder, familyPartSeparator)
	return slices.Contains(toolComponentSegments, segment)
}

func (lookup sourceTimeLookup) toolProjections(mixed bool) (toolProjection, toolProjection) {
	grouped := make(map[[2]int]int, len(lookup.toolCalls))
	actual := make(map[[2]int]int, len(lookup.toolCalls))
	for position, call := range lookup.toolCalls {
		grouped[[2]int{call.GroupedMessage, call.GroupedOrdinal}] = position
		actual[[2]int{call.ActualMessage, call.ActualOrdinal}] = position
	}
	var groupedOther, actualOther []transcript.UserTurn
	if mixed {
		groupedOther, actualOther = lookup.actualUsers, lookup.groupedUsers
	}
	return toolProjection{name: toolProjectionGrouped, calls: lookup.toolCalls, positions: grouped, users: lookup.groupedUsers, otherUsers: groupedOther, known: nil, residual: false},
		toolProjection{name: toolProjectionActual, calls: lookup.toolCalls, positions: actual, users: lookup.actualUsers, otherUsers: actualOther, known: nil, residual: false}
}

// transcriptProjection matches transcript calls to native calls by name and
// complete arguments through ProvenToolPositions. It records every transcript
// coordinate and assigns native positions only to proven matches.
func (lookup sourceTimeLookup) transcriptProjection() toolProjection {
	saved := make([]transcript.ToolIdentity, 0, len(lookup.transcript.Calls))
	for _, call := range lookup.transcript.Calls {
		saved = append(saved, transcript.ToolIdentity{Name: call.Name, Input: call.Input})
	}
	native := make([]transcript.ToolIdentity, 0, len(lookup.toolCalls))
	for _, call := range lookup.toolCalls {
		native = append(native, transcript.ToolIdentity{Name: call.Name, Input: call.Input})
	}
	proven := transcript.ProvenToolPositions(saved, native)
	positions := make(map[[2]int]int, len(saved))
	known := make(map[[2]int]bool, len(saved))
	unproven := 0
	for index, call := range lookup.transcript.Calls {
		coordinate := [2]int{call.Message, call.Ordinal}
		known[coordinate] = true
		if proven[index] == transcript.UnprovenToolPosition {
			unproven++
			continue
		}
		positions[coordinate] = proven[index]
	}
	if unproven > 0 {
		slog.Warn("conversation.vectorsearch.transcript_tool_occurrence_unproven",
			"concern", "conversation.semantic", "component", "conversation",
			"transcript_tools", len(saved), "native_tools", len(native), "unproven", unproven)
	}
	return toolProjection{name: toolProjectionTranscript, calls: lookup.toolCalls, positions: positions, users: lookup.transcript.Users, otherUsers: lookup.actualUsers, known: known, residual: false}
}

func (lookup sourceTimeLookup) bindBaseFamilies(keys []string, stored toolProjection, projections []toolProjection, storedUsers []storedUserMessage, families map[string][]collection.StoredRow) {
	storedVerified, storedDifference := stored.verifiedPrefix(keys, storedUsers, families)
	if storedDifference.check != "" {
		logToolSequenceDifference(stored.name, storedDifference, keys, len(lookup.toolCalls))
	}
	candidates := make(map[string][]int, len(keys))
	candidateProjection := make(map[string]string, len(keys))
	contentMismatch := make(map[string]bool)
	residual := make([]string, 0, len(keys))
	for _, key := range keys {
		if !stored.knows(key) {
			residual = append(residual, key)
		}
	}
	for _, projection := range projections {
		projection.residual = len(stored.known) > 0
		verified, difference := projection.verifiedPrefix(residual, storedUsers, families)
		for key, position := range verified {
			if !slices.Contains(candidates[key], position) {
				candidates[key] = append(candidates[key], position)
				candidateProjection[key] = projection.name
			}
		}
		if difference.check == toolSequenceCheckContent {
			contentMismatch[residual[difference.position]] = true
		}
		if difference.check != "" {
			logToolSequenceDifference(projection.name, difference, residual, len(lookup.toolCalls))
		}
	}
	olderRun := lookup.olderRunPositions(residual, candidates, families)
	for _, key := range keys {
		binding := toolBinding{call: emptySourceToolTime(), reason: SourceOutcomeSequenceMismatch, component: false, projection: ""}
		position, verified := storedVerified[key]
		switch {
		case verified:
			binding.call = lookup.toolCalls[position]
			binding.reason = ""
			binding.projection = stored.name
		case stored.knows(key):
			if storedDifference.check == toolSequenceCheckContent && keys[storedDifference.position] == key {
				binding.reason = SourceOutcomeContentMismatch
			}
		case len(candidates[key]) == 1:
			binding.call = lookup.toolCalls[candidates[key][0]]
			binding.reason = ""
			binding.projection = candidateProjection[key]
		case hasOlderRunPosition(olderRun, key):
			binding.call = lookup.toolCalls[olderRun[key]]
			binding.reason = ""
			binding.projection = toolProjectionOlderRun
		case len(candidates[key]) == 0 && contentMismatch[key]:
			binding.reason = SourceOutcomeContentMismatch
		}
		lookup.toolBindings[key] = binding
	}
}

func (lookup sourceTimeLookup) olderRunPositions(keys []string, candidates map[string][]int, families map[string][]collection.StoredRow) map[string]int {
	native := make([]string, 0, len(lookup.toolCalls))
	for _, call := range lookup.toolCalls {
		native = append(native, toolCallContent(call))
	}
	stored := make([]string, 0, len(keys))
	suffixStart := 0
	for index, key := range keys {
		text, complete := familyText(key, conversationToolFamily, families[key])
		if !complete || strings.TrimSpace(text) == "" {
			suffixStart = index + 1
		}
		stored = append(stored, text)
	}
	for from := suffixStart; len(keys)-from >= minimumOlderRunRows; from++ {
		start, unique := uniqueContiguousStart(stored[from:], native)
		if !unique {
			continue
		}
		positions := make(map[string]int, len(keys)-from)
		for index, key := range keys[from:] {
			position := start + index
			for _, candidate := range candidates[key] {
				if candidate != position {
					return nil
				}
			}
			positions[key] = position
		}
		return positions
	}
	return nil
}

func hasOlderRunPosition(positions map[string]int, key string) bool {
	_, found := positions[key]
	return found
}

func uniqueContiguousStart(stored []string, native []string) (int, bool) {
	found := -1
	for start := 0; start+len(stored) <= len(native); start++ {
		if !slices.Equal(stored, native[start:start+len(stored)]) {
			continue
		}
		if found >= 0 {
			return -1, false
		}
		found = start
	}
	return found, found >= 0
}

func (lookup sourceTimeLookup) bindComponentFamilies(keys []string, projection toolProjection, storedUsers []storedUserMessage, families map[string][]collection.StoredRow) {
	stopped := false
	previous := emptySourceToolTime()
	for index, key := range keys {
		binding := toolBinding{call: emptySourceToolTime(), reason: SourceOutcomeSequenceMismatch, component: true, projection: ""}
		if !stopped {
			position, found := projection.position(key)
			native := emptySourceToolTime()
			check, verified := toolSequenceCheckPosition, false
			if found {
				native = lookup.toolCalls[position]
				check = projection.unmatchedUserCheck(index, keys, storedUsers)
			}
			if found && check == "" {
				check, verified = componentCheck(key, families[key], native)
			}
			switch {
			case check != "":
				stopped = true
				logToolSequenceDifference(projection.name, toolSequenceDifference{check: check, position: index, storedName: storedToolName(families[key]), native: native, previous: previous}, keys, len(lookup.toolCalls))
			case verified:
				binding.call = native
				binding.reason = ""
				binding.projection = projection.name
			default:
				binding.reason = SourceOutcomeComponentUnevaluated
			}
			previous = native
		}
		lookup.toolBindings[key] = binding
	}
}

// componentCheck requires one token row and one complete input row before
// comparing their content and any command row with the native call. It reports
// an unevaluated result when the stored components do not support that
// comparison.
func componentCheck(key string, rows []collection.StoredRow, call SourceToolTime) (string, bool) {
	var tokenRows, commandRows, inputRows []collection.StoredRow
	for _, row := range rows {
		remainder, _ := strings.CutPrefix(row.RelativePath, key+familyPartSeparator)
		switch {
		case row.RelativePath == key:
			return "", false
		case remainder == toolComponentTokens:
			tokenRows = append(tokenRows, row)
		case remainder == toolComponentCommand:
			commandRows = append(commandRows, row)
		case strings.HasPrefix(remainder, toolComponentInput+familyPartSeparator):
			inputRows = append(inputRows, row)
		case !strings.HasPrefix(remainder, "out"+familyPartSeparator):
			return "", false
		}
	}
	if len(tokenRows) != 1 || len(inputRows) != 1 || inputRows[0].RelativePath != key+familyPartSeparator+toolComponentInput+"/0" {
		return "", false
	}
	command := historicalToolCommand(call)
	switch {
	case command == "" && len(commandRows) != 0:
		return toolSequenceCheckComponents, false
	case command != "" && (len(commandRows) != 1 || strings.TrimSpace(commandRows[0].Content) != command):
		return toolSequenceCheckComponents, false
	}
	input := inputRows[0].Content
	if !json.Valid([]byte(input)) || !transcript.SameToolInput(json.RawMessage(input), call.Input) {
		return toolSequenceCheckComponents, false
	}
	if tokenRows[0].Content != historicalToolTokens(call.Name, command, input) {
		return toolSequenceCheckComponents, false
	}
	return "", true
}

// historicalToolCommand returns the shell command the historical producer stored.
func historicalToolCommand(call SourceToolTime) string {
	if call.LangHint != bashLangHint {
		return ""
	}
	return strings.TrimSpace(call.Display)
}

// historicalToolTokens builds the token text used to compare older component
// rows with a native call.
func historicalToolTokens(name string, command string, input string) string {
	tokens := make([]string, 0)
	add := func(value string) {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			tokens = append(tokens, trimmed)
		}
	}
	add(name)
	if command != "" {
		addHistoricalShellTokens(add, command)
	}
	if input != "" {
		add(truncateHistoricalSummary(input))
	}
	return strings.Join(tokens, toolContinuationBreak)
}

func addHistoricalShellTokens(add func(string), command string) {
	decomposition := shelldecomp.Parse(command, "/", "")
	if decomposition == nil || decomposition.IsOpaque() {
		add(truncateHistoricalSummary(command))
		return
	}
	added := 0
	addCounted := func(value string) {
		if strings.TrimSpace(value) != "" {
			added++
		}
		add(value)
	}
	for _, shellCommand := range decomposition.Commands() {
		addCounted(shellCommand.Argv0)
	}
	addTarget := func(resolvable bool, path string, raw string) {
		if !resolvable {
			addCounted(raw)
			return
		}
		addCounted(path)
		if strings.TrimSpace(raw) != "" && strings.TrimSpace(raw) != strings.TrimSpace(path) {
			addCounted(raw)
		}
	}
	for _, target := range decomposition.ReadTargets() {
		addTarget(target.Resolvable, target.Path, target.Raw)
	}
	for _, target := range decomposition.WriteTargets() {
		addTarget(target.Resolvable, target.Path, target.Raw)
	}
	if added == 0 {
		add(truncateHistoricalSummary(command))
	}
}

func truncateHistoricalSummary(value string) string {
	if len(value) <= historicalToolSummaryBytes {
		return value
	}
	end := historicalToolSummaryBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

// knows reports whether the transcript contains the family's message and tool
// coordinate, including coordinates without a proven native position.
func (projection toolProjection) knows(key string) bool {
	order := familyOrder(key)
	return projection.known[[2]int{order[0], order[2]}]
}

func (projection toolProjection) position(key string) (int, bool) {
	order := familyOrder(key)
	position, found := projection.positions[[2]int{order[0], order[2]}]
	return position, found
}

// verifiedPrefix returns matching family positions up to the first name,
// content, or user turn difference and skips families without a mapped
// position.
func (projection toolProjection) verifiedPrefix(keys []string, storedUsers []storedUserMessage, families map[string][]collection.StoredRow) (map[string]int, toolSequenceDifference) {
	verified := make(map[string]int, len(keys))
	previous := emptySourceToolTime()
	previousKey := ""
	for index, key := range keys {
		position, found := projection.position(key)
		if !found {
			continue
		}
		native := projection.calls[position]
		check := projection.familyCheck(index, keys, storedUsers, native, families[key])
		if check == "" && projection.name == toolProjectionTranscript && previousKey != "" {
			check = projection.transcriptUserCheck(previousKey, key, previous, native, storedUsers)
		}
		if check != "" {
			return verified, toolSequenceDifference{check: check, position: index, storedName: storedToolName(families[key]), native: native, previous: previous}
		}
		verified[key] = position
		previous = native
		previousKey = key
	}
	return verified, toolSequenceDifference{check: "", position: len(keys), storedName: "", native: emptySourceToolTime(), previous: previous}
}

func (projection toolProjection) familyCheck(index int, keys []string, storedUsers []storedUserMessage, call SourceToolTime, rows []collection.StoredRow) string {
	if check := projection.unmatchedUserCheck(index, keys, storedUsers); check != "" {
		return check
	}
	if storedToolName(rows) != strings.TrimSpace(call.Name) {
		return toolSequenceCheckName
	}
	text, complete := familyText(keys[index], conversationToolFamily, rows)
	if complete && strings.TrimSpace(text) != "" && text != toolCallContent(call) {
		return toolSequenceCheckContent
	}
	return ""
}

// unmatchedUserCheck rejects a stored user row between consecutive tool rows
// unless the projection contains a user turn at that message position or
// another projection matches both its position and text. It does not check the
// transcript projection or projections that check only rows without transcript
// coordinates.
func (projection toolProjection) unmatchedUserCheck(index int, keys []string, storedUsers []storedUserMessage) string {
	if index == 0 || projection.name == toolProjectionTranscript || projection.residual {
		return ""
	}
	previous := familyOrder(keys[index-1])[0]
	current := familyOrder(keys[index])[0]
	for _, stored := range storedUsers {
		if stored.message <= previous || stored.message >= current {
			continue
		}
		matches := func(turn transcript.UserTurn) bool { return turn.Message == stored.message }
		associated := func(turn transcript.UserTurn) bool {
			return turn.Message == stored.message && slices.Contains(stored.texts, strings.TrimSpace(turn.Text))
		}
		if !slices.ContainsFunc(projection.users, matches) && !slices.ContainsFunc(projection.otherUsers, associated) {
			return toolSequenceCheckStoredUser
		}
	}
	return ""
}

// transcriptUserCheck requires a native user turn between the calls only for an
// intervening stored user row that matches a transcript user turn in both
// message position and text.
func (projection toolProjection) transcriptUserCheck(previousKey string, key string, previous SourceToolTime, current SourceToolTime, storedUsers []storedUserMessage) string {
	low := familyOrder(previousKey)[0]
	high := familyOrder(key)[0]
	for _, stored := range storedUsers {
		if stored.message <= low || stored.message >= high {
			continue
		}
		associated := slices.ContainsFunc(projection.users, func(turn transcript.UserTurn) bool {
			return turn.Message == stored.message && slices.Contains(stored.texts, strings.TrimSpace(turn.Text))
		})
		if !associated {
			continue
		}
		between := slices.ContainsFunc(projection.otherUsers, func(turn transcript.UserTurn) bool {
			return turn.Message > previous.ActualMessage && turn.Message < current.ActualMessage
		})
		if !between {
			return toolSequenceCheckStoredUser
		}
	}
	return ""
}

func storedToolName(rows []collection.StoredRow) string {
	for _, row := range rows {
		if strings.HasSuffix(row.RelativePath, familyPartSeparator+toolComponentTokens) {
			name, _, _ := strings.Cut(row.Content, toolContinuationBreak)
			return strings.TrimSpace(name)
		}
	}
	name, _, _ := strings.Cut(rows[0].Content, toolContinuationBreak)
	return strings.TrimSpace(name)
}

// storedUserMessages returns stored user text families and their matching texts.
func (lookup sourceTimeLookup) storedUserMessages(families map[string][]collection.StoredRow) []storedUserMessage {
	users := make([]storedUserMessage, 0)
	for key, rows := range families {
		if !strings.HasPrefix(key, conversationTextFamily+familyPartSeparator) || !strings.EqualFold(rowRole(rows), storedUserRole) {
			continue
		}
		users = append(users, storedUserMessage{message: familyOrder(key)[0], texts: lookup.storedUserTexts(key, rows)})
	}
	return users
}

// storedUserTexts returns normalized texts for matching a stored user family,
// joining complete parts or using each row separately when parts are
// incomplete.
func (lookup sourceTimeLookup) storedUserTexts(key string, rows []collection.StoredRow) []string {
	contents := make([]string, 0, len(rows))
	if text, complete := familyText(key, conversationTextFamily, rows); complete {
		contents = append(contents, text)
	} else {
		for _, row := range rows {
			contents = append(contents, row.Content)
		}
	}
	texts := make([]string, 0, len(contents))
	for _, content := range contents {
		if lookup.matchText != nil {
			content = lookup.matchText(storedUserRole, content)
		}
		texts = append(texts, strings.TrimSpace(content))
	}
	return texts
}

// storedUserRole identifies stored text rows with the user role.
const storedUserRole = "user"

func logToolSequenceDifference(projection string, difference toolSequenceDifference, keys []string, nativeTools int) {
	previousFamily, storedFamily := "", ""
	if difference.position > 0 && difference.position <= len(keys) {
		previousFamily = keys[difference.position-1]
	}
	if difference.position < len(keys) {
		storedFamily = keys[difference.position]
	}
	slog.Warn("conversation.vectorsearch.tool_sequence_mismatch",
		"concern", "conversation.semantic", "component", "conversation",
		"projection", projection, "check", difference.check, "position", difference.position,
		"stored_tools", len(keys), "native_tools", nativeTools,
		"previous_family", previousFamily, "stored_family", storedFamily,
		"stored_name", difference.storedName, "native_name", difference.native.Name,
		"previous_call_id", difference.previous.CallID, "native_call_id", difference.native.CallID,
		"previous_message_start", difference.previous.MessageStart, "previous_after_user", difference.previous.AfterUser,
		"message_start", difference.native.MessageStart, "after_user", difference.native.AfterUser)
}

func emptySourceToolTime() SourceToolTime {
	return SourceToolTime{CallID: "", Input: nil, Name: "", Display: "", LangHint: "", TimestampUnix: missingTimestampUnix, Reason: "", MessageStart: false, AfterUser: false, GroupedMessage: 0, GroupedOrdinal: 0, ActualMessage: 0, ActualOrdinal: 0}
}

// toolCallContent renders the stored content of one native tool call.
func toolCallContent(call SourceToolTime) string {
	return toolContent(semsearch.SemToolCall{
		Name: strings.ToValidUTF8(call.Name, ""), Display: strings.ToValidUTF8(call.Display, ""),
		LangHint: strings.ToValidUTF8(call.LangHint, ""), Output: "", IsError: false, TimestampUnix: nil, TimestampUnknown: false,
	})
}

// toolFamilyOutcome checks a tool family's binding, content, and timestamp reason.
func (lookup sourceTimeLookup) toolFamilyOutcome(key string, text string, evidence *SourceRepairEvidence) {
	binding, found := lookup.toolBindings[key]
	if !found {
		evidence.Outcome = SourceOutcomeSequenceMismatch
		return
	}
	if binding.reason == "" && toolCallContent(binding.call) != text {
		evidence.Outcome = SourceOutcomeContentMismatch
		return
	}
	bindingOutcome(binding, evidence)
}

// bindingOutcome records a binding failure, an unavailable date, or the matched date.
func bindingOutcome(binding toolBinding, evidence *SourceRepairEvidence) {
	if binding.reason != "" {
		evidence.Outcome = binding.reason
		return
	}
	evidence.ToolCallID = binding.call.CallID
	evidence.ToolProjection = binding.projection
	if binding.call.Reason != "" {
		evidence.Outcome = binding.call.Reason
		return
	}
	evidence.Outcome = SourceOutcomeMatched
	evidence.Candidates = []int64{binding.call.TimestampUnix}
	evidence.Chosen = binding.call.TimestampUnix
	evidence.Match = sourceMatchNativeIdentity
}
