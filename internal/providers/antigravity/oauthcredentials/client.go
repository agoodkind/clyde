package oauthcredentials

// Scopes returns the permissions requested by the installed OAuth client.
//
// Each entry identifies one Google API permission.
//
//	permissions := Scopes()
func Scopes() []string {
	return []string{
		"https://www.googleapis.com/auth/cloud-platform",
		"https://www.googleapis.com/auth/userinfo.email",
		"https://www.googleapis.com/auth/userinfo.profile",
		"https://www.googleapis.com/auth/cclog",
		"https://www.googleapis.com/auth/experimentsandconfigs",
	}
}
