// Package antigravity implements Clyde's Gemini backend for Antigravity logins.
// It owns Code Assist request and response types, project discovery, token
// refresh, and Gemini streaming translation. The daemon injects credentials
// through CredentialSource. This package does not read provider files.
//
// A caller builds a request with BuildGenerateContent, wraps it with
// NewCodeAssistEnvelope, and sends it through Client.Stream.
package antigravity
