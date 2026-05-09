package session_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"goodkind.io/clyde/internal/session"
)

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
