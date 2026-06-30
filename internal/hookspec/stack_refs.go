package hookspec

var (
	_ = shellCommand
	_ = shellQuote
	_ = isShellBareWord
)

func init() {
	_ = shellCommand("", nil)
}
