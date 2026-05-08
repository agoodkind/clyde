//go:build !darwin && !linux

package truststore

// unsupportedRegistry is the no-op implementation used on platforms
// that have no trust-store integration in this build (notably
// Windows). Every method returns ErrUnsupportedPlatform so the CLI
// surfaces a uniform setup error.
type unsupportedRegistry struct{}

func newPlatformRegistry() Registry {
	return unsupportedRegistry{}
}

// Platform reports the unsupported identity so the CLI can render the
// reason without inspecting runtime.GOOS itself.
func (unsupportedRegistry) Platform() PlatformID {
	return PlatformUnsupported
}

func (unsupportedRegistry) CACommonName() string {
	return CACommonName
}

func (unsupportedRegistry) Status(_ string) (InstallStatus, error) {
	return InstallStatus{
		Platform:   PlatformUnsupported,
		CommonName: CACommonName,
	}, ErrUnsupportedPlatform
}

func (unsupportedRegistry) Install(_ string) error {
	return ErrUnsupportedPlatform
}

func (unsupportedRegistry) Uninstall() error {
	return ErrUnsupportedPlatform
}
