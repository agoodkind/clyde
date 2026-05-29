package providerid

import (
	"encoding/json"
	"testing"
)

func TestProviderJSONUsesStableLabel(t *testing.T) {
	type record struct {
		Provider Provider `json:"provider"`
	}
	input := record{Provider: ProviderClaude}

	data, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal provider record: %v", err)
	}
	if string(data) != `{"provider":"claude"}` {
		t.Fatalf("provider JSON = %s, want string label", string(data))
	}
}
