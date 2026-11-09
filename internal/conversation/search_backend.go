package conversation

import "fmt"

// SearchBackend selects the store for one search. Empty uses the configured source.
type SearchBackend string

const (
	// SearchBackendMilvus queries Milvus with the configured embedding settings.
	SearchBackendMilvus SearchBackend = "milvus"
	// SearchBackendLocal queries the existing local index.
	SearchBackendLocal SearchBackend = "local"
)

// Validate rejects unsupported search backends.
//
// Empty, Milvus, and local values pass validation. Other values return an error
// containing the invalid value.
//
//	SearchBackendLocal.Validate() // Returns nil.
//	SearchBackend("other").Validate() // Returns an error.
func (backend SearchBackend) Validate() error {
	switch backend {
	case "", SearchBackendMilvus, SearchBackendLocal:
		return nil
	default:
		return fmt.Errorf("invalid search backend %q: use milvus or local", backend)
	}
}
