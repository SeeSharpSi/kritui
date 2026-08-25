package kritui_db

import (
	"context"
	"testing"

	"seesharpsi/kritui/llm"
)

func TestModelEndpointTypeRoundTrip(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := context.Background()

	endpointType, err := GetModelEndpointType(ctx, database, "model-a")
	if err != nil {
		t.Fatalf("GetModelEndpointType() unset error: %v", err)
	}
	if endpointType != "" {
		t.Errorf("unset endpoint type = %q, want empty", endpointType)
	}

	for _, want := range []llm.EndpointType{llm.EndpointMessages, llm.EndpointChatCompletions} {
		if err := SetModelEndpointType(ctx, database, " model-a ", want); err != nil {
			t.Fatalf("SetModelEndpointType(%q) error: %v", want, err)
		}
		got, err := GetModelEndpointType(ctx, database, "model-a")
		if err != nil {
			t.Fatalf("GetModelEndpointType() error: %v", err)
		}
		if got != want {
			t.Errorf("endpoint type = %q, want %q", got, want)
		}
	}
}

func TestModelEndpointTypeRejectsInvalidValues(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := context.Background()

	if err := SetModelEndpointType(ctx, database, "", llm.EndpointResponses); err == nil {
		t.Error("SetModelEndpointType() accepted empty model")
	}
	if err := SetModelEndpointType(ctx, database, "model", llm.EndpointType("invalid")); err == nil {
		t.Error("SetModelEndpointType() accepted invalid endpoint type")
	}
	if _, err := database.Exec(`INSERT INTO model_endpoint_preferences (model, endpoint_type) VALUES ('model', 'invalid')`); err == nil {
		t.Error("model_endpoint_preferences accepted invalid endpoint type")
	}
}
