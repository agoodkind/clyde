package mitm

// TunnelMeta is the per-tunnel metadata stored alongside each
// livetrack.Session the proxy registers. The fields are the
// minimum set the daemon and the operator-facing tooling need to
// answer "which tunnel is this and where is it pointed" without
// dragging the rest of the proxy state into every snapshot.
//
// IsLivetrackMeta is the empty marker method the livetrack package
// requires; its only purpose is to constrain the registry's type
// parameter at compile time.
type TunnelMeta struct {
	ConnectHost   string
	UpstreamAddr  string
	CaptureFile   string
	KeepaliveSeen bool
}

// IsLivetrackMeta is the empty marker method that satisfies the
// livetrack.Meta constraint. The body is intentionally empty.
func (TunnelMeta) IsLivetrackMeta() {}
