package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

type (
	noticeClaimer   func(kind string, resetsAt time.Time) bool
	noticeUnclaimer func(kind string, resetsAt time.Time)
	encodeJSON      func(any) ([]byte, error)
)

// EvaluateNoticeFromHeaders checks the Anthropic headers and claims a notice slot.
func EvaluateNoticeFromHeaders(h http.Header, noticesEnabled bool, usageThresholdsUsedPercent []float64, claim noticeClaimer) *anthropic.Notice {
	if !noticesEnabled {
		return nil
	}
	notice := anthropic.EvaluateNotice(h, runtimeClock.Now().UTC(), usageThresholdsUsedPercent)
	if notice == nil || claim == nil {
		return nil
	}
	if strings.TrimSpace(notice.Text) == "" {
		return nil
	}
	if !claim(notice.Kind, notice.ResetsAt) {
		return nil
	}
	return notice
}

// noticeSentinelText wraps an upstream rate-limit / usage notice in
// HTML-comment sentinels so shared sentinel cleanup can remove the
// whole envelope from chat history on the next turn (keeping the
// Anthropic-side cache prefix byte-stable). The visible payload is
// a compact markdown blockquote with a small-font inner span so the
// notice reads as a subtle UI affordance rather than shouting at
// the user. The leading blank line isolates the notice from any
// preceding assistant content.
func noticeSentinelText(text string) string {
	return fmt.Sprintf(
		"<!--clyde-notice-->\n> <sub>⚡ %s</sub>\n<!--/clyde-notice-->\n\n",
		text,
	)
}

func openAINoticeChunk(reqID, modelAlias, text string) adapteropenai.StreamChunk {
	return adapteropenai.StreamChunk{
		ID:      reqID,
		Object:  "chat.completion.chunk",
		Created: runtimeClock.Now().Unix(),
		Model:   modelAlias,
		Choices: []adapteropenai.StreamChoice{{
			Index: 0,
			Delta: adapteropenai.StreamDelta{
				Role:    "assistant",
				Content: text,
			},
		}},
	}
}

// NoticeForStreamHeaders evaluates headers, emits one notice chunk, and
// unclaims the slot when emission fails.
func NoticeForStreamHeaders(
	reqID string,
	modelAlias string,
	h http.Header,
	noticesEnabled bool,
	usageThresholdsUsedPercent []float64,
	emit func(adapteropenai.StreamChunk) error,
	claim noticeClaimer,
	unclaim noticeUnclaimer,
) (*anthropic.Notice, error) {
	if emit == nil {
		return nil, nil
	}
	notice := EvaluateNoticeFromHeaders(h, noticesEnabled, usageThresholdsUsedPercent, claim)
	if notice == nil {
		return nil, nil
	}
	chunk := openAINoticeChunk(reqID, modelAlias, noticeSentinelText(notice.Text))
	if err := emit(chunk); err != nil {
		if unclaim != nil {
			unclaim(notice.Kind, notice.ResetsAt)
		}
		return notice, err
	}
	return nil, nil
}

// NoticeForResponseHeaders injects a notice into the first assistant message and
// unclaims the slot when injection fails.
func NoticeForResponseHeaders(
	resp ChatResponse,
	notice *anthropic.Notice,
	unclaim noticeUnclaimer,
	encode encodeJSON,
) (ChatResponse, bool) {
	return prependNoticeToResponse(resp, notice, unclaim, encode)
}

func prependNoticeToResponse(resp ChatResponse, notice *anthropic.Notice, unclaim noticeUnclaimer, encode encodeJSON) (ChatResponse, bool) {
	if notice == nil || len(resp.Choices) == 0 {
		return resp, false
	}
	var content string
	if err := json.Unmarshal(resp.Choices[0].Message.Content, &content); err != nil {
		if unclaim != nil {
			unclaim(notice.Kind, notice.ResetsAt)
		}
		return resp, false
	}
	content = noticeSentinelText(notice.Text) + content
	if encode == nil {
		encode = json.Marshal
	}
	encoded, err := encode(content)
	if err != nil {
		if unclaim != nil {
			unclaim(notice.Kind, notice.ResetsAt)
		}
		return resp, false
	}
	resp.Choices[0].Message.Content = json.RawMessage(encoded)
	return resp, true
}
