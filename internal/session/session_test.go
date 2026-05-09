package session_test

import (
	"encoding/json"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"goodkind.io/clyde/internal/session"
)

var _ = Describe("Session", func() {
	Describe("NewSession", func() {
		It("should create a new session with given name and UUID", func() {
			name := "test-session"
			sessionID := "550e8400-e29b-41d4-a716-446655440000"

			s := session.NewSession(name, sessionID)

			Expect(s.Name).To(Equal(name))
			Expect(s.Metadata.Name).To(Equal(name))
			Expect(s.ProviderID()).To(Equal(session.ProviderClaude))
			Expect(s.Metadata.ProviderSessionID()).To(Equal(sessionID))
			Expect(s.Metadata.Created).To(BeTemporally("~", time.Now(), time.Second))
			Expect(s.Metadata.LastAccessed).To(BeTemporally("~", time.Now(), time.Second))
			Expect(s.Metadata.IsForkedSession).To(BeFalse())
			Expect(s.Metadata.ParentSession).To(BeEmpty())
		})
	})

	Describe("UpdateLastAccessed", func() {
		It("should update the lastAccessed timestamp", func() {
			s := session.NewSession("test", "uuid")
			originalTime := s.Metadata.LastAccessed

			time.Sleep(10 * time.Millisecond)
			s.UpdateLastAccessed()

			Expect(s.Metadata.LastAccessed).To(BeTemporally(">", originalTime))
			Expect(s.Metadata.LastAccessed).To(BeTemporally("~", time.Now(), time.Second))
		})
	})

	Describe("Identity", func() {
		It("defaults legacy metadata to the Claude provider", func() {
			s := &session.Session{
				Name: "legacy",
				Metadata: session.Metadata{
					Name:      "legacy",
					SessionID: "legacy-uuid",
				},
			}

			identity := s.Identity()
			Expect(identity.Current.Provider).To(Equal(session.ProviderClaude))
			Expect(identity.Current.ID).To(Equal("legacy-uuid"))
			Expect(session.IdentityKey(s)).To(Equal("provider:claude:sid:legacy-uuid"))
		})

		It("treats provider namespaces as distinct identity domains", func() {
			claude := &session.Session{
				Name: "claude-session",
				Metadata: session.Metadata{
					Name:      "claude-session",
					Provider:  session.ProviderClaude,
					SessionID: "shared-id",
				},
			}
			other := &session.Session{
				Name: "other-session",
				Metadata: session.Metadata{
					Name:      "other-session",
					Provider:  session.ProviderID("other"),
					SessionID: "shared-id",
				},
			}

			Expect(session.IdentityKey(claude)).ToNot(Equal(session.IdentityKey(other)))
		})

		It("records previous ids through provider-aware rotation", func() {
			s := session.NewSession("rotating", "uuid-1")

			s.RotateIdentity(session.ProviderSessionID{
				Provider: session.ProviderClaude,
				ID:       "uuid-2",
			})
			s.RotateIdentity(session.ProviderSessionID{
				Provider: session.ProviderClaude,
				ID:       "uuid-2",
			})

			Expect(s.Metadata.ProviderSessionID()).To(Equal("uuid-2"))
			Expect(s.Metadata.PreviousSessionIDs).To(Equal([]string{"uuid-1"}))
			Expect(s.Identity().HasID("uuid-1")).To(BeTrue())
			Expect(s.Identity().HasID("uuid-2")).To(BeTrue())
		})

		It("normalizes legacy metadata into provider-owned state", func() {
			md := session.Metadata{
				Name:               "legacy",
				SessionID:          "uuid-current",
				PreviousSessionIDs: []string{"uuid-previous"},
				TranscriptPath:     "/tmp/transcript.jsonl",
			}

			md.NormalizeProviderState()

			Expect(md.ProviderState).ToNot(BeNil())
			Expect(md.ProviderState.Current).To(Equal(session.ProviderSessionID{
				Provider: session.ProviderClaude,
				ID:       "uuid-current",
			}))
			Expect(md.ProviderState.Previous).To(Equal([]session.ProviderSessionID{{
				Provider: session.ProviderClaude,
				ID:       "uuid-previous",
			}}))
			Expect(md.ProviderState.Artifacts.TranscriptPath).To(Equal("/tmp/transcript.jsonl"))
			Expect(md.ProviderSessionID()).To(Equal("uuid-current"))
			Expect(md.PreviousProviderSessionIDStrings()).To(Equal([]string{"uuid-previous"}))
			Expect(md.ProviderTranscriptPath()).To(Equal("/tmp/transcript.jsonl"))
		})

		It("mirrors provider-owned state back to legacy fields", func() {
			md := session.Metadata{
				Name: "provider-owned",
				ProviderState: &session.ProviderOwnedMetadata{
					Current: session.ProviderSessionID{
						Provider: session.ProviderID("codex"),
						ID:       "codex-current",
					},
					Previous: []session.ProviderSessionID{{
						Provider: session.ProviderID("codex"),
						ID:       "codex-previous",
					}},
					Artifacts: session.ProviderArtifacts{
						TranscriptPath: "/tmp/codex.jsonl",
					},
				},
			}

			md.NormalizeProviderState()

			Expect(md.Provider).To(Equal(session.ProviderID("codex")))
			Expect(md.ProviderSessionID()).To(Equal("codex-current"))
			Expect(md.PreviousSessionIDs).To(Equal([]string{"codex-previous"}))
			Expect(md.ProviderTranscriptPath()).To(Equal("/tmp/codex.jsonl"))
		})

		It("applies the metadata provider to unqualified provider-owned identities", func() {
			md := session.Metadata{
				Name:     "codex-provider-owned",
				Provider: session.ProviderCodex,
				ProviderState: &session.ProviderOwnedMetadata{
					Current: session.ProviderSessionID{ID: "codex-current"},
					Previous: []session.ProviderSessionID{{
						ID: "codex-previous",
					}},
				},
			}

			md.NormalizeProviderState()

			Expect(md.Provider).To(Equal(session.ProviderCodex))
			Expect(md.ProviderState.Current).To(Equal(session.ProviderSessionID{
				Provider: session.ProviderCodex,
				ID:       "codex-current",
			}))
			Expect(md.ProviderState.Previous).To(Equal([]session.ProviderSessionID{{
				Provider: session.ProviderCodex,
				ID:       "codex-previous",
			}}))
			Expect(md.SessionID).To(Equal("codex-current"))
			Expect(md.PreviousSessionIDs).To(Equal([]string{"codex-previous"}))
		})

		It("rotates unqualified next identities inside the current provider namespace", func() {
			s := &session.Session{
				Name: "codex-rotating",
				Metadata: session.Metadata{
					Name:      "codex-rotating",
					Provider:  session.ProviderCodex,
					SessionID: "codex-current",
				},
			}
			s.Metadata.NormalizeProviderState()

			s.RotateIdentity(session.ProviderSessionID{ID: "codex-next"})

			Expect(s.ProviderID()).To(Equal(session.ProviderCodex))
			Expect(s.Metadata.ProviderState.Current).To(Equal(session.ProviderSessionID{
				Provider: session.ProviderCodex,
				ID:       "codex-next",
			}))
			Expect(s.Metadata.ProviderState.Previous).To(Equal([]session.ProviderSessionID{{
				Provider: session.ProviderCodex,
				ID:       "codex-current",
			}}))
		})
	})

	Describe("Auto-name metadata fields", func() {
		It("omits the auto-name string fields when they hold zero values", func() {
			md := session.Metadata{
				Name:      "zero-fields",
				SessionID: "uuid-zero",
				Created:   time.Unix(1700000000, 0).UTC(),
			}

			raw, err := json.Marshal(md)
			Expect(err).NotTo(HaveOccurred())

			// The two enum-string fields use omitempty so a zero value
			// keeps the legacy on-disk shape of metadata.json.
			Expect(strings.Contains(string(raw), "autoNameState")).To(BeFalse())
			Expect(strings.Contains(string(raw), "autoNameSource")).To(BeFalse())
		})

		It("round-trips populated auto-name state, source, and timestamp", func() {
			cases := []struct {
				state  session.AutoNameState
				source session.AutoNameSource
			}{
				{session.AutoNameStateApplied, session.AutoNameSourceTranscript},
				{session.AutoNameStateApplied, session.AutoNameSourceLLM},
				{session.AutoNameStateApplied, session.AutoNameSourceDefault},
				{session.AutoNameStateUserLocked, session.AutoNameSourceUser},
			}
			for _, tc := range cases {
				original := session.Metadata{
					Name:           "round-trip",
					SessionID:      "uuid-rt",
					Created:        time.Unix(1700000000, 0).UTC(),
					AutoNameState:  tc.state,
					AutoNameSource: tc.source,
					LastAutoNameAt: time.Unix(1700001234, 0).UTC(),
				}

				raw, err := json.Marshal(original)
				Expect(err).NotTo(HaveOccurred())

				var decoded session.Metadata
				Expect(json.Unmarshal(raw, &decoded)).To(Succeed())
				Expect(decoded.AutoNameState).To(Equal(tc.state))
				Expect(decoded.AutoNameSource).To(Equal(tc.source))
				Expect(decoded.LastAutoNameAt.Equal(original.LastAutoNameAt)).To(BeTrue())

				rawAgain, err := json.Marshal(decoded)
				Expect(err).NotTo(HaveOccurred())
				Expect(rawAgain).To(Equal(raw))
			}
		})

		It("decodes legacy metadata that omits the auto-name fields", func() {
			legacy := []byte(`{"name":"legacy","sessionId":"uuid-legacy","created":"2023-01-01T00:00:00Z","lastAccessed":"2023-01-01T00:00:00Z","isForkedSession":false,"isIncognito":false}`)

			var md session.Metadata
			Expect(json.Unmarshal(legacy, &md)).To(Succeed())
			Expect(md.AutoNameState).To(Equal(session.AutoNameStateUntouched))
			Expect(md.AutoNameSource).To(Equal(session.AutoNameSourceUnspecified))
			Expect(md.LastAutoNameAt.IsZero()).To(BeTrue())
		})
	})

	Describe("Provider capabilities", func() {
		It("keeps Claude as the full-featured default provider", func() {
			caps := session.ProviderInfo(session.ProviderClaude).Capabilities
			Expect(caps.ResumeByID).To(BeTrue())
			Expect(caps.ForkByID).To(BeTrue())
			Expect(caps.PerSessionSettings).To(BeTrue())
			Expect(caps.RemoteControl).To(BeTrue())
			Expect(caps.TranscriptTail).To(BeTrue())
			Expect(caps.TranscriptExport).To(BeTrue())
			Expect(caps.Compaction).To(BeTrue())
			Expect(caps.ContextUsageInspect).To(BeTrue())
		})

		It("advertises only conservative Codex session capabilities", func() {
			caps := session.ProviderInfo(session.ProviderCodex).Capabilities
			Expect(caps.ResumeByID).To(BeTrue())
			Expect(caps.ForkByID).To(BeFalse())
			Expect(caps.PerSessionSettings).To(BeFalse())
			Expect(caps.RemoteControl).To(BeFalse())
			Expect(caps.TranscriptTail).To(BeFalse())
			Expect(caps.TranscriptExport).To(BeFalse())
			Expect(caps.Compaction).To(BeFalse())
			Expect(caps.ContextUsageInspect).To(BeFalse())
		})
	})
})
