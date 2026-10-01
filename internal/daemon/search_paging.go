package daemon

import (
	"goodkind.io/clyde/internal/conversation"
)

const maxInt32Value = 2147483647

type invalidSearchBoundsError string

func (e invalidSearchBoundsError) Error() string {
	return string(e)
}

func normalizedPagingOffset(rawOffset int) int {
	if rawOffset < 0 {
		return 0
	}
	if rawOffset > maxInt32Value {
		return maxInt32Value
	}
	return rawOffset
}

func normalizedSearchLimit(rawLimit int) int {
	if rawLimit <= 0 {
		return conversation.DefaultSearchLimit
	}
	if rawLimit > conversation.MaxSearchLimit {
		return conversation.MaxSearchLimit
	}
	return rawLimit
}
