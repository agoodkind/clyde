package session_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"goodkind.io/clyde/internal/session"
)

var _ = Describe("SanitizeLegacySlugName", func() {
	DescribeTable("produces a valid legacy slug alias or empty string",
		func(raw string, expected string) {
			got := session.SanitizeLegacySlugName(raw)
			Expect(got).To(Equal(expected))
			if got != "" {
				Expect(session.ValidateLegacySlugName(got)).To(Succeed(), "sanitize output %q must pass ValidateLegacySlugName", got)
			}
		},
		Entry("empty input", "", ""),
		Entry("already valid slug", "merry-swan", "merry-swan"),
		Entry("date prefix slug", "2026-04-12-merry-swan", "2026-04-12-merry-swan"),
		Entry("uppercase lowers", "MerrySwan", "merryswan"),
		Entry("spaces become hyphens", "my chat name", "my-chat-name"),
		Entry("multiple punctuation collapses", "foo --- bar", "foo-bar"),
		Entry("edge hyphens trimmed", "-foo-", "foo"),
		Entry("emoji only returns empty", "🙂🎉", ""),
		Entry("mixed non ascii drops to ascii core", "café-plan", "caf-plan"),
		Entry("underscores become hyphens", "foo_bar_baz", "foo-bar-baz"),
		Entry("long string truncates to 64 chars", "a-very-long-name-that-exceeds-the-sixty-four-character-maximum-by-a-lot", "a-very-long-name-that-exceeds-the-sixty-four-character-maximum-b"),
		Entry("trailing hyphen after truncation trims", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-------bbb", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbb"),
	)

	It("keeps Sanitize as a deprecated compatibility wrapper", func() {
		Expect(session.Sanitize("Merry Swan")).To(Equal("merry-swan"))
	})
})

var _ = Describe("UniqueLegacySlugName", func() {
	It("returns the base when it is not taken", func() {
		taken := map[string]bool{"other": true}
		Expect(session.UniqueLegacySlugName("foo", taken)).To(Equal("foo"))
	})

	It("appends a numeric suffix when the base collides", func() {
		taken := map[string]bool{"foo": true}
		Expect(session.UniqueLegacySlugName("foo", taken)).To(Equal("foo-2"))
	})

	It("keeps climbing past the first collision", func() {
		taken := map[string]bool{"foo": true, "foo-2": true, "foo-3": true}
		Expect(session.UniqueLegacySlugName("foo", taken)).To(Equal("foo-4"))
	})

	It("returns empty when the base is empty", func() {
		Expect(session.UniqueLegacySlugName("", map[string]bool{})).To(Equal(""))
	})

	It("keeps UniqueName as a deprecated compatibility wrapper", func() {
		Expect(session.UniqueName("foo", map[string]bool{"foo": true})).To(Equal("foo-2"))
	})
})

var _ = Describe("UniqueDisplayName", func() {
	It("returns the exact base when it is not taken", func() {
		taken := map[string]bool{"Other": true}
		Expect(session.UniqueDisplayName("Merry Swan", taken)).To(Equal("Merry Swan"))
	})

	It("appends a human-visible suffix when the exact base collides", func() {
		taken := map[string]bool{"Merry Swan": true}
		Expect(session.UniqueDisplayName("Merry Swan", taken)).To(Equal("Merry Swan (2)"))
	})

	It("keeps climbing past display-name collisions", func() {
		taken := map[string]bool{
			"Merry Swan":     true,
			"Merry Swan (2)": true,
			"Merry Swan (3)": true,
		}
		Expect(session.UniqueDisplayName("Merry Swan", taken)).To(Equal("Merry Swan (4)"))
	})

	It("returns empty for invalid display names", func() {
		Expect(session.UniqueDisplayName("line\nbreak", map[string]bool{})).To(Equal(""))
	})
})
