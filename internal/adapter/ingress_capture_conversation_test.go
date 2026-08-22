package adapter

import (
	"testing"

	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/gklog/correlation"
)

// TestIngressConversationFieldsOnlyStoresExportableIDs pins the contract for
// the conversation_id column on adapter ingress capture rows: it holds an id
// that `clyde conversation export <id>` resolves, or nothing.
//
// The Cursor chat identity resolver returns a native id when Cursor sent
// conversation metadata, and otherwise a derived lineage key that groups
// related requests but names no conversation. Only the native one belongs here.
func TestIngressConversationFieldsOnlyStoresExportableIDs(t *testing.T) {
	t.Parallel()

	const nativeID = "2e26c0f6-c33d-4b54-93a4-96c94b0b7b11"

	tests := []struct {
		name       string
		chatKey    string
		source     string
		wantID     string
		wantSource string
	}{
		{
			name:       "native id becomes a derived clyde conversation id",
			chatKey:    nativeID,
			source:     clydeingress.ChatKeySourceNative,
			wantID:     "cursor:" + nativeID,
			wantSource: clydeingress.ChatKeySourceNative,
		},
		{
			name:       "derived lineage key is not stored",
			chatKey:    "a1b2c3d4.b01",
			source:     clydeingress.ChatKeySourceDerived,
			wantID:     "",
			wantSource: "",
		},
		{
			name:       "no chat identity stores nothing",
			chatKey:    "",
			source:     "",
			wantID:     "",
			wantSource: "",
		},
		{
			name:       "native source with an empty key stores nothing",
			chatKey:    "",
			source:     clydeingress.ChatKeySourceNative,
			wantID:     "",
			wantSource: "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			corr := correlation.Context{}
			if test.chatKey != "" || test.source != "" {
				corr = clydeingress.WithChatIdentity(corr, test.chatKey, test.source, test.chatKey, "")
			}
			gotID, gotSource := ingressConversationFields(corr)
			if gotID != test.wantID {
				t.Fatalf("conversation id = %q, want %q", gotID, test.wantID)
			}
			if gotSource != test.wantSource {
				t.Fatalf("conversation source = %q, want %q", gotSource, test.wantSource)
			}
		})
	}
}
