package kritui_db

import "testing"

func TestToolNameJSONNormalizesEmptyLists(t *testing.T) {
	encoded, err := encodeToolNames(nil)
	if err != nil {
		t.Fatalf("encodeToolNames() error: %v", err)
	}
	if encoded != "[]" {
		t.Errorf("encodeToolNames(nil) = %q, want []", encoded)
	}

	decoded, err := decodeToolNames("null")
	if err != nil {
		t.Fatalf("decodeToolNames() error: %v", err)
	}
	if decoded == nil || len(decoded) != 0 {
		t.Errorf("decodeToolNames(null) = %#v, want non-nil empty slice", decoded)
	}
}

func TestDecodeToolNamesRejectsMalformedJSON(t *testing.T) {
	if _, err := decodeToolNames("["); err == nil {
		t.Fatal("decodeToolNames() error = nil, want malformed JSON error")
	}
}
