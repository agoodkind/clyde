package contextusage

import (
	"goodkind.io/clyde/internal/contextusage"
)

// Source re-exports the generic Source so existing call sites in
// this package compile without importing the generic package
// directly. New code should reference contextusage.Source.
type Source = contextusage.Source

// Source constants alias the generic ones so call sites that pass
// SourceProbe, SourceCacheMem, or SourceCacheDisk keep compiling
// without importing the generic package directly.
const (
	SourceProbe     = contextusage.SourceProbe
	SourceCacheMem  = contextusage.SourceCacheMem
	SourceCacheDisk = contextusage.SourceCacheDisk
)

// Category aliases the generic shape. The generic package owns the
// type; the Claude package adapts onto it.
type Category = contextusage.Category

// Usage aliases the generic Usage. Lives here as an alias so existing
// callers continue to compile; new code should reference
// contextusage.Usage directly.
type Usage = contextusage.Usage
