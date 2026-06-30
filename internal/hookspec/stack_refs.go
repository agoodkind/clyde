package hookspec

var (
	_ = shellCommand
	_ = shellQuote
	_ = isShellBareWord
	_ = SupportedClients
)

func init() {
	_ = shellCommand("", nil)
}
