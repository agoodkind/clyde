package util_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"goodkind.io/clyde/internal/util"
)

var _ = Describe("FormatRelativeTime", func() {
	It("should return 'just now' for times less than a minute ago", func() {
		now := time.Now()
		Expect(util.FormatRelativeTime(now)).To(Equal("just now"))
		Expect(util.FormatRelativeTime(now.Add(-30 * time.Second))).To(Equal("just now"))
		Expect(util.FormatRelativeTime(now.Add(-59 * time.Second))).To(Equal("just now"))
	})

	It("should return '1 minute ago' for times exactly 1 minute ago", func() {
		t := time.Now().Add(-1 * time.Minute)
		Expect(util.FormatRelativeTime(t)).To(Equal("1 minute ago"))
	})

	It("should return 'X minutes ago' for times 2-59 minutes ago", func() {
		Expect(util.FormatRelativeTime(time.Now().Add(-2 * time.Minute))).To(Equal("2 minutes ago"))
		Expect(util.FormatRelativeTime(time.Now().Add(-5 * time.Minute))).To(Equal("5 minutes ago"))
		Expect(util.FormatRelativeTime(time.Now().Add(-30 * time.Minute))).To(Equal("30 minutes ago"))
		Expect(util.FormatRelativeTime(time.Now().Add(-59 * time.Minute))).To(Equal("59 minutes ago"))
	})

	It("should return '1 hour ago' for times exactly 1 hour ago", func() {
		t := time.Now().Add(-1 * time.Hour)
		Expect(util.FormatRelativeTime(t)).To(Equal("1 hour ago"))
	})

	It("should return 'X hours ago' for times 2-23 hours ago", func() {
		Expect(util.FormatRelativeTime(time.Now().Add(-2 * time.Hour))).To(Equal("2 hours ago"))
		Expect(util.FormatRelativeTime(time.Now().Add(-12 * time.Hour))).To(Equal("12 hours ago"))
		Expect(util.FormatRelativeTime(time.Now().Add(-23 * time.Hour))).To(Equal("23 hours ago"))
	})

	It("should return '1 day ago' for times exactly 1 day ago", func() {
		t := time.Now().Add(-24 * time.Hour)
		Expect(util.FormatRelativeTime(t)).To(Equal("1 day ago"))
	})

	It("should return 'X days ago' for times 2-6 days ago", func() {
		Expect(util.FormatRelativeTime(time.Now().Add(-2 * 24 * time.Hour))).To(Equal("2 days ago"))
		Expect(util.FormatRelativeTime(time.Now().Add(-3 * 24 * time.Hour))).To(Equal("3 days ago"))
		Expect(util.FormatRelativeTime(time.Now().Add(-6 * 24 * time.Hour))).To(Equal("6 days ago"))
	})

	It("should return date format for times 7+ days ago", func() {
		// 7 days ago
		t := time.Now().Add(-7 * 24 * time.Hour)
		result := util.FormatRelativeTime(t)
		Expect(result).To(MatchRegexp(`^\d{4}-\d{2}-\d{2}$`))

		// Verify the date is correct
		expectedDate := t.Format("2006-01-02")
		Expect(result).To(Equal(expectedDate))
	})

	It("should return date format for times months ago", func() {
		t := time.Now().Add(-90 * 24 * time.Hour)
		result := util.FormatRelativeTime(t)
		Expect(result).To(MatchRegexp(`^\d{4}-\d{2}-\d{2}$`))
	})
})
