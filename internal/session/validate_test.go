package session_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"goodkind.io/clyde/internal/session"
)

var _ = Describe("ValidateLegacySlugName", func() {
	Context("valid names", func() {
		It("should accept simple lowercase names", func() {
			validNames := []string{
				"ab",
				"test",
				"feature",
				"my-session",
				"bug-123",
				"feature-auth-v2",
			}

			for _, name := range validNames {
				err := session.ValidateLegacySlugName(name)
				Expect(err).NotTo(HaveOccurred(), "name %s should be valid", name)
			}
		})

		It("should accept names with numbers", func() {
			validNames := []string{
				"test123",
				"123test",
				"v1-alpha",
				"feature-v2-123",
			}

			for _, name := range validNames {
				err := session.ValidateLegacySlugName(name)
				Expect(err).NotTo(HaveOccurred(), "name %s should be valid", name)
			}
		})

		It("should accept maximum length names", func() {
			name := "a123456789012345678901234567890123456789012345678901234567890123"
			Expect(len(name)).To(Equal(64))
			err := session.ValidateLegacySlugName(name)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("invalid names", func() {
		It("should reject names that are too short", func() {
			err := session.ValidateLegacySlugName("a")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("at least 2 characters"))
		})

		It("should reject names that are too long", func() {
			name := "a1234567890123456789012345678901234567890123456789012345678901234"
			Expect(len(name)).To(Equal(65))
			err := session.ValidateLegacySlugName(name)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("at most 64 characters"))
		})

		It("should reject names with uppercase letters", func() {
			invalidNames := []string{
				"MySession",
				"FEATURE",
				"Test-Session",
			}

			for _, name := range invalidNames {
				err := session.ValidateLegacySlugName(name)
				Expect(err).To(HaveOccurred(), "name %s should be invalid", name)
			}
		})

		It("should reject names starting or ending with hyphen", func() {
			invalidNames := []string{
				"-test",
				"test-",
				"-",
				"--",
			}

			for _, name := range invalidNames {
				err := session.ValidateLegacySlugName(name)
				Expect(err).To(HaveOccurred(), "name %s should be invalid", name)
			}
		})

		It("should reject names with consecutive hyphens", func() {
			invalidNames := []string{
				"test--session",
				"my--feature",
				"a--b",
			}

			for _, name := range invalidNames {
				err := session.ValidateLegacySlugName(name)
				Expect(err).To(HaveOccurred(), "name %s should be invalid", name)
				Expect(err.Error()).To(ContainSubstring("consecutive hyphens"))
			}
		})

		It("should reject names with special characters", func() {
			invalidNames := []string{
				"test_session",
				"my.session",
				"session@123",
				"test session",
				"test/session",
			}

			for _, name := range invalidNames {
				err := session.ValidateLegacySlugName(name)
				Expect(err).To(HaveOccurred(), "name %s should be invalid", name)
			}
		})
	})

	It("keeps ValidateName as a deprecated compatibility wrapper", func() {
		Expect(session.ValidateName("legacy-slug")).To(Succeed())
	})
})

var _ = Describe("ValidateDisplayName", func() {
	It("accepts exact human-visible names without slugifying", func() {
		validNames := []string{
			"Merry Swan",
			"Feature/Auth v2",
			"café plan",
			"🙂 pair-debugging",
		}

		for _, name := range validNames {
			err := session.ValidateDisplayName(name)
			Expect(err).NotTo(HaveOccurred(), "display name %q should be valid", name)
		}
	})

	It("rejects names that cannot be matched exactly", func() {
		invalidNames := []string{
			"",
			" ",
			" leading",
			"trailing ",
			"line\nbreak",
		}

		for _, name := range invalidNames {
			err := session.ValidateDisplayName(name)
			Expect(err).To(HaveOccurred(), "display name %q should be invalid", name)
		}
	})
})
