package adapter

import "bytes"

const (
	passthroughTerminalResponseEvent   = "response.completed"
	passthroughIncompleteResponseEvent = "response.incomplete"
)

type passthroughTerminalEvent uint8

const (
	passthroughTerminalEventNone passthroughTerminalEvent = iota
	passthroughTerminalEventCompleted
	passthroughTerminalEventIncomplete
)

type passthroughSSELineKind uint8

const (
	passthroughSSELineUnknown passthroughSSELineKind = iota
	passthroughSSELineData
	passthroughSSELineEvent
)

func passthroughTerminalEventForName(name []byte) passthroughTerminalEvent {
	switch {
	case bytes.Equal(name, []byte(passthroughTerminalResponseEvent)):
		return passthroughTerminalEventCompleted
	case bytes.Equal(name, []byte(passthroughIncompleteResponseEvent)):
		return passthroughTerminalEventIncomplete
	default:
		return passthroughTerminalEventNone
	}
}
