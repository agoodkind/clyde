package daemon

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/transcript"
)

// SemanticProjectionHash hashes one conversation's projected documents: every
// byte the engine would store, in delivery order. Two projections hash equal
// exactly when the engine would receive identical content, so the feeder can
// tell a real content change from an artifact whose stamp moved with unchanged
// bytes. Field and document boundaries are length-prefixed so concatenation
// ambiguity cannot collide two different projections.
func SemanticProjectionHash(docs []semsearch.SemDoc) string {
	hasher := sha256.New()
	writeField := func(value string) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		hasher.Write(length[:])
		hasher.Write([]byte(value))
	}
	for _, doc := range docs {
		writeField(strconv.Itoa(int(doc.MessageIndex)))
		writeField(doc.Role)
		writeField(doc.Text)
		writeField(doc.Thinking)
		for _, tool := range doc.Tools {
			writeField(tool.Name)
			writeField(tool.Display)
			writeField(tool.Output)
		}
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// SemanticConversationLoadOptions maps the selected content kinds onto the
// parser's load gate, so a kind nobody selected is never parsed rather than
// parsed and discarded. Four of the nine kinds have a matching field; the rest
// are applied when the documents are projected.
func SemanticConversationLoadOptions(kinds conversation.ContentKindSet) conversation.LoadOptions {
	return conversation.LoadOptions{
		IncludeSystemPrompts:  kinds.Has(conversation.ContentKindSystemPrompts),
		IncludeSystemMessages: kinds.Has(conversation.ContentKindSystemMessages),
		IncludeToolOutputs:    kinds.Has(conversation.ContentKindToolOutputs),
		IncludeInjected:       kinds.Has(conversation.ContentKindInjected),
		HarnessTally:          nil,
	}
}

// SemanticConversationDocuments is one conversation's projection: the documents
// offered to the engine, and how many messages the content policy withheld.
//
// The two counts stay separate all the way to the caller because they mean
// opposite things. A withheld message is the policy working, while a conversation
// that fails to load is content lost, and a counter that mixed them would hide
// real loss behind routine policy.
type SemanticConversationDocuments struct {
	Docs          []semsearch.SemDoc
	PolicySkipped int
	// InjectedStripped and SystemStripped total what the provider parsers
	// removed or withheld while loading this conversation, taken from the load
	// tally rather than from surviving messages, so a record removed entirely
	// still counts. The loader fills them; the projection does not.
	InjectedStripped int
	SystemStripped   int
}

// BuildSemanticConversationDocuments projects loaded transcript messages into
// the document shape sent to lm-semantic-search, keeping only the content the
// policy names.
//
// A message whose every indexed class is empty is not offered at all. That rule
// is derived rather than configured: there is nothing to retrieve, so there is
// no setting under which offering it would be right. It is deliberately not the
// same as "the text is empty", because a turn carrying only a tool call or only
// reasoning has no text and still has content, and skipping on empty text would
// discard it.
//
// A skipped message keeps its index rather than renumbering the ones after it.
// The message index is a position in this same loaded slice, and the search path
// feeds an engine hit's index back into the loader as a position, so renumbering
// would silently shift every later hit's context window.
func BuildSemanticConversationDocuments(
	record conversation.Record,
	messages []transcript.Message,
	kinds conversation.ContentKindSet,
) (SemanticConversationDocuments, error) {
	parentConversationID := ""
	if derivedParentID, ok := conversation.ParentConversationID(record); ok {
		parentConversationID = derivedParentID
	}
	built := SemanticConversationDocuments{
		Docs:             make([]semsearch.SemDoc, 0, len(messages)),
		PolicySkipped:    0,
		InjectedStripped: 0,
		SystemStripped:   0,
	}
	for i, message := range messages {
		if i > int(maxSemanticMessageIndex) {
			return SemanticConversationDocuments{Docs: nil, PolicySkipped: 0, InjectedStripped: 0, SystemStripped: 0},
				fmt.Errorf("message index %d exceeds semantic search int32 limit", i)
		}
		// A control record is the harness talking to itself rather than
		// anything a person wrote or read, so embedding it spends the same
		// work as a real message and returns a result nobody searched for.
		// Reading already withholds these, in messageCountsForLastN and in
		// conversation info, and this is that judgement applied to the feed.
		// A transcript-only record is the same judgement one step removed: it
		// is content the harness shows in a transcript view but the person
		// never saw as chat, and the compact-summary records it marks quote an
		// entire earlier session, so embedding one duplicates the whole
		// conversation into rows nobody searched for.
		if message.Visibility == transcript.MessageVisibilityMetaOnly ||
			message.Visibility == transcript.MessageVisibilityTranscriptOnly {
			built.PolicySkipped++
			continue
		}
		text := ""
		if kinds.Has(conversation.ContentKindChat) {
			// Replace invalid UTF-8 so the protobuf upsert never fails to marshal
			// on a transcript byte sequence the encoder rejects (one codex doc
			// with invalid UTF-8 used to break the whole batch).
			text = strings.ToValidUTF8(message.Text, "")
			// Text that holds only spacing carries nothing a search could return,
			// so offer it as no text at all. The receiving contract carries text
			// as a plain string, where an unset field and an empty one are the
			// same bytes, so absence is expressible only as empty, and a single
			// space is content on the wire that would be stored as an
			// unreturnable row.
			//
			// Only text that is entirely spacing is replaced. Trimming text that
			// has content would make every already-stored message differ from its
			// newly offered form, and the receiver re-embeds a message whose text
			// changed, so the whole collection would be embedded again for no
			// gain.
			if strings.TrimSpace(text) == "" {
				text = ""
			}
		}
		thinking := ""
		if kinds.Has(conversation.ContentKindThinking) {
			thinking = strings.ToValidUTF8(message.Thinking, "")
		}
		tools := semanticToolCalls(message.Tools, kinds)
		if text == "" && thinking == "" && len(tools) == 0 {
			built.PolicySkipped++
			continue
		}
		built.Docs = append(built.Docs, semsearch.SemDoc{
			ConversationID:       record.ID,
			ParentConversationID: parentConversationID,
			MessageIndex:         int32(i),
			Role:                 message.Role,
			TimestampUnix:        message.Timestamp.Unix(),
			Text:                 text,
			Tools:                tools,
			Thinking:             thinking,
			WorkspaceRoot:        record.WorkspaceRoot,
			Archived:             record.Archived,
		})
	}
	return built, nil
}

// semanticToolCalls projects a message's tool calls at the selected detail level.
//
// The three tool kinds are nested rather than parallel, and
// [conversation.NewContentKindSet] collapses them, so exactly one applies: the
// summary level carries the tool's name alone, the call level adds what the user
// saw, and the output level adds what the tool returned. Selecting
// no tool kind drops the calls entirely.
//
// The projection stays structured so the engine can store each call separately.
func semanticToolCalls(tools []transcript.ToolCall, kinds conversation.ContentKindSet) []semsearch.SemToolCall {
	summariesOnly := kinds.Has(conversation.ContentKindToolSummaries)
	withArguments := kinds.Has(conversation.ContentKindToolCalls)
	withOutput := kinds.Has(conversation.ContentKindToolOutputs)
	if !summariesOnly && !withArguments && !withOutput {
		return nil
	}
	out := make([]semsearch.SemToolCall, 0, len(tools))
	for _, tool := range tools {
		projected := semsearch.SemToolCall{
			Name:     tool.Name,
			Display:  "",
			LangHint: "",
			Output:   "",
			IsError:  tool.IsError,
		}
		if withArguments || withOutput {
			// The provider's parser rendered what the user saw and named the
			// language it is written in. Re-deriving either here would put
			// knowledge of every harness's tool shapes into a layer that must
			// not hold it.
			projected.Display = strings.ToValidUTF8(tool.Display, "")
			projected.LangHint = tool.DisplayLang
		}
		if withOutput {
			projected.Output = tool.Output
		}
		out = append(out, projected)
	}
	return out
}
