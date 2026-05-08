package slogger

// Session concern constants.
const (
	ConcernSessionDomainStore        = "session.domain.store"
	ConcernSessionDomainResolve      = "session.domain.resolve"
	ConcernSessionDomainSearch       = "session.domain.search"
	ConcernSessionDomainCapabilities = "session.domain.capabilities"
	ConcernSessionLifecycleLaunch    = "session.lifecycle.launch"
	ConcernSessionLifecycleRuntime   = "session.lifecycle.runtime"
	ConcernSessionLifecycleCleanup   = "session.lifecycle.cleanup"
	ConcernSessionDiscoveryScan      = "session.discovery.scan"
	ConcernSessionDiscoveryAdopt     = "session.discovery.adopt"
)

func init() {
	registerConcernPaths(map[string]string{
		ConcernSessionDomainStore:        "session/domain/store.jsonl",
		ConcernSessionDomainResolve:      "session/domain/resolve.jsonl",
		ConcernSessionDomainSearch:       "session/domain/search.jsonl",
		ConcernSessionDomainCapabilities: "session/domain/capabilities.jsonl",
		ConcernSessionLifecycleLaunch:    "session/lifecycle/launch.jsonl",
		ConcernSessionLifecycleRuntime:   "session/lifecycle/runtime.jsonl",
		ConcernSessionLifecycleCleanup:   "session/lifecycle/cleanup.jsonl",
		ConcernSessionDiscoveryScan:      "session/discovery/scan.jsonl",
		ConcernSessionDiscoveryAdopt:     "session/discovery/adopt.jsonl",
	})

	registerEventConcernRules([]eventConcernRule{
		{"session.scan.", ConcernSessionDiscoveryScan},
		{"session.adopt.", ConcernSessionDiscoveryAdopt},
		{"session.resolve.", ConcernSessionDomainResolve},
		{"session.store.", ConcernSessionDomainStore},
		{"session.list.", ConcernSessionDomainSearch},
		{"session.search.", ConcernSessionDomainSearch},
		{"session.context.", ConcernSessionDomainCapabilities},
		{"session.lifecycle.", ConcernSessionLifecycleRuntime},
		{"session.cleanup.", ConcernSessionLifecycleCleanup},
		{"session.new.", ConcernSessionLifecycleLaunch},
		{"session.resume.", ConcernSessionLifecycleLaunch},
		{"resume.start", ConcernSessionLifecycleLaunch},
		{"resume.exit", ConcernSessionLifecycleLaunch},
	})
}
