package conversation

import (
	"goodkind.io/clyde/internal/transcript"
)

// Info is the typed static summary for one conversation artifact.
type Info struct {
	Record          Record
	Stats           Stats
	CompactionCount int
	Segments        []CompactionSegment
}

// Stats contains message and tool counts computed from the raw
// transcript artifact.
type Stats struct {
	TotalMessages     int
	VisibleMessages   int
	UserMessages      int
	AssistantMessages int
	SystemMessages    int
	ToolCallCount     int
	ToolOutputCount   int
}

type infoMessageRole string

const (
	infoMessageRoleUser      infoMessageRole = "user"
	infoMessageRoleAssistant infoMessageRole = "assistant"
	infoMessageRoleSystem    infoMessageRole = "system"
)

// ConversationInfo loads one artifact and computes metadata that does not
// require rendering the transcript body.
func (idx *Index) ConversationInfo(record Record) (Info, error) {
	messages, err := idx.LoadMessagesWithOptions(record, LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: true,
		IncludeToolOutputs:    true,
	})
	if err != nil {
		return Info{}, err
	}
	segments := CompactionSegments(messages)
	return Info{
		Record:          record,
		Stats:           conversationStats(messages),
		CompactionCount: len(CompactionCheckpoints(messages)),
		Segments:        segments,
	}, nil
}

func conversationStats(messages []transcript.Message) Stats {
	stats := Stats{
		TotalMessages:     len(messages),
		VisibleMessages:   0,
		UserMessages:      0,
		AssistantMessages: 0,
		SystemMessages:    0,
		ToolCallCount:     0,
		ToolOutputCount:   0,
	}
	for _, message := range messages {
		if message.Compaction == nil && message.Visibility != transcript.MessageVisibilityMetaOnly {
			stats.VisibleMessages++
		}
		switch infoMessageRole(message.Role) {
		case infoMessageRoleUser:
			stats.UserMessages++
		case infoMessageRoleAssistant:
			stats.AssistantMessages++
		case infoMessageRoleSystem:
			stats.SystemMessages++
		default:
			// Other provider roles are counted in total and visible counts only.
		}
		stats.ToolCallCount += len(message.Tools)
		for _, tool := range message.Tools {
			if tool.Output != "" {
				stats.ToolOutputCount++
			}
		}
	}
	return stats
}
