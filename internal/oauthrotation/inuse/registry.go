package inuse

import (
	"sync"

	"goodkind.io/clyde/internal/oauthrotation/provider"
)

// registryState holds the process-wide detector registry. The package keeps
// it as a single package-level value so providers can register themselves
// from an init or a setup function without threading a registry handle
// through the rotator.
type registryState struct {
	mu        sync.RWMutex
	detectors map[provider.Name]Detector
}

var globalRegistry = &registryState{
	mu:        sync.RWMutex{},
	detectors: make(map[provider.Name]Detector),
}

// Register associates detector with name in the process-wide registry. A
// later Register for the same name replaces the previous detector. Passing
// nil is treated as a clear so test setup can revert to NoopDetector
// semantics on the next Lookup.
func Register(name provider.Name, detector Detector) {
	globalRegistry.mu.Lock()
	defer globalRegistry.mu.Unlock()
	if detector == nil {
		delete(globalRegistry.detectors, name)
		return
	}
	globalRegistry.detectors[name] = detector
}

// Lookup returns the detector registered under name. When no detector is
// registered the NoopDetector is returned so the caller can use the result
// without a nil check.
func Lookup(name provider.Name) Detector {
	globalRegistry.mu.RLock()
	detector, ok := globalRegistry.detectors[name]
	globalRegistry.mu.RUnlock()
	if !ok {
		return NoopDetector{}
	}
	return detector
}
