package claude

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/daemonclient"
)

// fakeReauthClient is a scripted oauthReauthClient that records the calls the
// reauth flow makes and returns canned results without a live daemon.
type fakeReauthClient struct {
	loginProvider  string
	loginErr       error
	loginAuthorize string
	loginAccount   string

	forgetProvider  string
	forgetIDOrLabel string
	forgotten       bool
	forgetAccountID string
	forgetErr       error
}

func (f *fakeReauthClient) Login(_ context.Context, providerName, _, _ string, onUpdate func(update daemonclient.OAuthLoginUpdate)) error {
	f.loginProvider = providerName
	if f.loginErr != nil {
		return f.loginErr
	}
	if f.loginAuthorize != "" {
		onUpdate(daemonclient.OAuthLoginUpdate{AuthorizeURL: f.loginAuthorize, Account: nil})
	}
	if f.loginAccount != "" {
		account := daemonclient.OAuthAccount{
			Provider:            providerName,
			AccountID:           f.loginAccount,
			Label:               "",
			ExpiresAtMS:         0,
			RefreshTokenPresent: true,
			ThrottledUntilMS:    0,
			ThrottledClaim:      "",
			NeedsReauth:         false,
		}
		onUpdate(daemonclient.OAuthLoginUpdate{AuthorizeURL: "", Account: &account})
	}
	return nil
}

func (f *fakeReauthClient) Forget(_ context.Context, providerName, idOrLabel string) (bool, string, error) {
	f.forgetProvider = providerName
	f.forgetIDOrLabel = idOrLabel
	if f.forgetErr != nil {
		return false, "", f.forgetErr
	}
	return f.forgotten, f.forgetAccountID, nil
}

// reauthTestHarness swaps every reauth flow seam for the duration of a test and
// restores the originals on cleanup.
type reauthTestHarness struct {
	in            *strings.Reader
	out           *bytes.Buffer
	client        *fakeReauthClient
	connectErr    error
	refetchResult daemon.ProviderLaunchEnvironment
	refetchErr    error
	refetchCalls  int
}

func installReauthHarness(t *testing.T, terminal bool, input string) *reauthTestHarness {
	t.Helper()
	harness := &reauthTestHarness{
		in:            strings.NewReader(input),
		out:           &bytes.Buffer{},
		client:        &fakeReauthClient{},
		connectErr:    nil,
		refetchResult: emptyLaunchEnv(),
		refetchErr:    nil,
		refetchCalls:  0,
	}

	oldIsTerminal := launchReauthIsTerminal
	oldIn := reauthPromptIn
	oldOut := reauthPromptOut
	oldConnect := connectReauthClient
	oldRefetch := reauthRefetchEnv
	t.Cleanup(func() {
		launchReauthIsTerminal = oldIsTerminal
		reauthPromptIn = oldIn
		reauthPromptOut = oldOut
		connectReauthClient = oldConnect
		reauthRefetchEnv = oldRefetch
	})

	launchReauthIsTerminal = func() bool { return terminal }
	reauthPromptIn = harness.in
	reauthPromptOut = harness.out
	connectReauthClient = func(context.Context) (oauthReauthClient, func(), error) {
		if harness.connectErr != nil {
			return nil, func() {}, harness.connectErr
		}
		return harness.client, func() {}, nil
	}
	reauthRefetchEnv = func(context.Context) (daemon.ProviderLaunchEnvironment, error) {
		harness.refetchCalls++
		return harness.refetchResult, harness.refetchErr
	}
	return harness
}

func needsReauthSignal() daemon.LaunchCredentialReauth {
	return daemon.LaunchCredentialReauth{
		Kind:             daemon.LaunchReauthNeedsReauth,
		Provider:         "anthropic",
		Account:          "acct-dead",
		ThrottledUntilMS: 0,
	}
}

func usableLaunchEnv() daemon.ProviderLaunchEnvironment {
	return daemon.ProviderLaunchEnvironment{
		Environment: []*clydev1.EnvironmentVariable{
			{Key: "CLAUDE_CONFIG_DIR", Value: "/tmp/scratch"},
		},
		Reauth: daemon.LaunchCredentialReauth{
			Kind:             daemon.LaunchReauthNone,
			Provider:         "",
			Account:          "",
			ThrottledUntilMS: 0,
		},
	}
}

// TestResolveLaunchReauthNoTTYFallsBack confirms a non-terminal launch never
// prompts and returns ok=false so the caller launches without planted creds.
func TestResolveLaunchReauthNoTTYFallsBack(t *testing.T) {
	harness := installReauthHarness(t, false, "")

	_, ok := resolveLaunchReauth(context.Background(), needsReauthSignal())

	if ok {
		t.Fatalf("ok = true for non-terminal launch, want false")
	}
	if harness.out.Len() != 0 {
		t.Fatalf("prompt rendered for non-terminal launch: %q", harness.out.String())
	}
	if harness.refetchCalls != 0 {
		t.Fatalf("refetch called %d times for non-terminal launch, want 0", harness.refetchCalls)
	}
}

// TestResolveLaunchReauthSkipFallsBack confirms choice 2 launches without
// planted creds and never re-fetches the launch env.
func TestResolveLaunchReauthSkipFallsBack(t *testing.T) {
	harness := installReauthHarness(t, true, "2\n")

	_, ok := resolveLaunchReauth(context.Background(), needsReauthSignal())

	if ok {
		t.Fatalf("ok = true for skip, want false")
	}
	if harness.refetchCalls != 0 {
		t.Fatalf("refetch called %d times for skip, want 0", harness.refetchCalls)
	}
	if !strings.Contains(harness.out.String(), "needs re-authentication") {
		t.Fatalf("prompt did not render the needs-reauth headline: %q", harness.out.String())
	}
}

// TestResolveLaunchReauthLoginReselectsAndPlants confirms choice 1 drives the
// login flow, prints the authorize URL, re-fetches, and returns the now-usable
// environment with ok=true.
func TestResolveLaunchReauthLoginReselectsAndPlants(t *testing.T) {
	harness := installReauthHarness(t, true, "1\n")
	harness.client.loginAuthorize = "https://authorize.test/url"
	harness.client.loginAccount = "acct-new"
	harness.refetchResult = usableLaunchEnv()

	launchEnv, ok := resolveLaunchReauth(context.Background(), needsReauthSignal())

	if !ok {
		t.Fatalf("ok = false after successful login + reselect, want true")
	}
	if harness.client.loginProvider != "anthropic" {
		t.Fatalf("login provider = %q, want anthropic", harness.client.loginProvider)
	}
	if harness.refetchCalls != 1 {
		t.Fatalf("refetch called %d times, want 1", harness.refetchCalls)
	}
	if len(launchEnv.Environment) != 1 || launchEnv.Environment[0].GetKey() != "CLAUDE_CONFIG_DIR" {
		t.Fatalf("planted env = %+v, want CLAUDE_CONFIG_DIR", launchEnv.Environment)
	}
	if !strings.Contains(harness.out.String(), "https://authorize.test/url") {
		t.Fatalf("authorize URL not printed: %q", harness.out.String())
	}
}

// TestResolveLaunchReauthLoginStillUnusableFallsBack confirms a login that
// leaves the rotator with no usable account falls back to no planted creds.
func TestResolveLaunchReauthLoginStillUnusableFallsBack(t *testing.T) {
	harness := installReauthHarness(t, true, "1\n")
	harness.client.loginAccount = "acct-new"
	harness.refetchResult = daemon.ProviderLaunchEnvironment{
		Environment: nil,
		Reauth:      needsReauthSignal(),
	}

	_, ok := resolveLaunchReauth(context.Background(), needsReauthSignal())

	if ok {
		t.Fatalf("ok = true when reselect still needs reauth, want false")
	}
	if harness.refetchCalls != 1 {
		t.Fatalf("refetch called %d times, want 1", harness.refetchCalls)
	}
	if !strings.Contains(harness.out.String(), "Still no usable account") {
		t.Fatalf("still-unusable message not printed: %q", harness.out.String())
	}
}

// TestResolveLaunchReauthRemoveForgetsAndReselects confirms choice 3 forgets the
// named account and re-fetches the launch env.
func TestResolveLaunchReauthRemoveForgetsAndReselects(t *testing.T) {
	harness := installReauthHarness(t, true, "3\n")
	harness.client.forgotten = true
	harness.client.forgetAccountID = "acct-dead"
	harness.refetchResult = usableLaunchEnv()

	launchEnv, ok := resolveLaunchReauth(context.Background(), needsReauthSignal())

	if !ok {
		t.Fatalf("ok = false after forget + reselect, want true")
	}
	if harness.client.forgetProvider != "anthropic" || harness.client.forgetIDOrLabel != "acct-dead" {
		t.Fatalf("forget called with provider=%q id=%q, want anthropic/acct-dead", harness.client.forgetProvider, harness.client.forgetIDOrLabel)
	}
	if len(launchEnv.Environment) != 1 {
		t.Fatalf("planted env = %+v, want one entry", launchEnv.Environment)
	}
}

// TestResolveLaunchReauthUnrecognizedChoiceFallsBack confirms an unrecognized
// entry falls back to no planted creds without calling login or forget.
func TestResolveLaunchReauthUnrecognizedChoiceFallsBack(t *testing.T) {
	harness := installReauthHarness(t, true, "9\n")

	_, ok := resolveLaunchReauth(context.Background(), needsReauthSignal())

	if ok {
		t.Fatalf("ok = true for unrecognized choice, want false")
	}
	if harness.client.loginProvider != "" || harness.client.forgetProvider != "" {
		t.Fatalf("login/forget called for unrecognized choice")
	}
}

// TestResolveLaunchReauthLoginErrorFallsBack confirms a failed login surfaces
// the error and falls back to no planted creds without re-fetching.
func TestResolveLaunchReauthLoginErrorFallsBack(t *testing.T) {
	harness := installReauthHarness(t, true, "1\n")
	harness.client.loginErr = errors.New("login child exited")

	_, ok := resolveLaunchReauth(context.Background(), needsReauthSignal())

	if ok {
		t.Fatalf("ok = true after login error, want false")
	}
	if harness.refetchCalls != 0 {
		t.Fatalf("refetch called %d times after login error, want 0", harness.refetchCalls)
	}
	if !strings.Contains(harness.out.String(), "Login failed") {
		t.Fatalf("login failure not surfaced: %q", harness.out.String())
	}
}
