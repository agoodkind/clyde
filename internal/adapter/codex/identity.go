package codex

import (
	"fmt"
	"runtime"
)

// CodexCLIVersion is the codex-cli release version Clyde reports in the
// spoofed `User-Agent` header on the Codex path. It tracks a published
// codex-cli release and can be bumped when Clyde tracks a newer one.
//
// The codex-rs workspace Cargo.toml in the local mirror carries a
// development placeholder ("0.0.0"), so the version is not read from the
// mirror; it is pinned here to a real release instead. Reference for the
// header shape: research/codex/codex-rs/login/src/auth/default_client.rs
// get_codex_user_agent uses env!("CARGO_PKG_VERSION").
const CodexCLIVersion = "0.51.0"

// codexTerminalUserAgent is the trailing terminal token in the codex-cli
// User-Agent. codex-rs computes it from terminal detection
// (research/codex/codex-rs/terminal-detection/src/lib.rs user_agent());
// when no terminal environment is detected the token is the literal
// "unknown" (TerminalName::Unknown). A detached daemon has no terminal,
// so "unknown" is the faithful value.
const codexTerminalUserAgent = "unknown"

// codexOSTypeToken is the os_info os_type() display string codex-rs emits
// in the User-Agent (research/codex/codex-rs uses the os_info crate).
type codexOSTypeToken string

const (
	codexOSTypeMacOS   codexOSTypeToken = "Mac OS"
	codexOSTypeLinux   codexOSTypeToken = "Linux"
	codexOSTypeWindows codexOSTypeToken = "Windows"
	codexOSTypeUnknown codexOSTypeToken = "Unknown"
)

// codexArchToken is the os_info architecture() token codex-rs emits in the
// User-Agent.
type codexArchToken string

const (
	codexArchARM64   codexArchToken = "arm64"
	codexArchX86_64  codexArchToken = "x86_64"
	codexArchX86     codexArchToken = "x86"
	codexArchUnknown codexArchToken = "unknown"
)

// codexOSVersionUnknown is the os version token os_info itself emits when
// it cannot determine a concrete version. Clyde does not probe the OS
// version, so it reports this value.
const codexOSVersionUnknown = "Unknown"

// UserAgent returns the spoofed codex-cli User-Agent string in the same
// shape codex-rs builds in
// research/codex/codex-rs/login/src/auth/default_client.rs
// get_codex_user_agent: "{originator}/{version} ({os_type} {os_version};
// {arch}) {terminal_user_agent}". The os_type and arch tokens are mapped
// from Go's runtime to the os_info-style strings codex emits.
func UserAgent() string {
	return fmt.Sprintf(
		"%s/%s (%s %s; %s) %s",
		CodexOriginatorValue,
		CodexCLIVersion,
		codexOSType(),
		codexOSVersionUnknown,
		codexArchitecture(),
		codexTerminalUserAgent,
	)
}

// codexOSType maps [runtime.GOOS] to the os_info os_type() display string
// codex-rs emits.
func codexOSType() codexOSTypeToken {
	switch codexGOOS(runtime.GOOS) {
	case goosDarwin:
		return codexOSTypeMacOS
	case goosLinux:
		return codexOSTypeLinux
	case goosWindows:
		return codexOSTypeWindows
	default:
		return codexOSTypeUnknown
	}
}

// codexArchitecture maps [runtime.GOARCH] to the os_info architecture()
// token codex-rs emits.
func codexArchitecture() codexArchToken {
	switch codexGOARCH(runtime.GOARCH) {
	case goarchARM64:
		return codexArchARM64
	case goarchAMD64:
		return codexArchX86_64
	case goarch386:
		return codexArchX86
	default:
		return codexArchUnknown
	}
}

// codexGOOS is the closed enum of [runtime.GOOS] values codex maps to an
// os_info os_type token.
type codexGOOS string

const (
	goosDarwin  codexGOOS = "darwin"
	goosLinux   codexGOOS = "linux"
	goosWindows codexGOOS = "windows"
)

// codexGOARCH is the closed enum of [runtime.GOARCH] values codex maps to
// an os_info architecture token.
type codexGOARCH string

const (
	goarchARM64 codexGOARCH = "arm64"
	goarchAMD64 codexGOARCH = "amd64"
	goarch386   codexGOARCH = "386"
)
