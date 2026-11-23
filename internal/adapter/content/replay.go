package content

// ReplayKind identifies a segment of an assistant reply.
type ReplayKind string

// ReplayOrigin identifies the provider family that produced a reasoning block.
type ReplayOrigin string

// ReplayPart represents one ordered segment of an assistant reply.
type ReplayPart struct {
	Kind      ReplayKind   `json:"kind"`
	Body      string       `json:"body,omitempty"`
	Ref       string       `json:"ref,omitempty"`
	Encrypted string       `json:"encrypted,omitempty"`
	Signature string       `json:"signature,omitempty"`
	Origin    ReplayOrigin `json:"origin,omitempty"`
}
