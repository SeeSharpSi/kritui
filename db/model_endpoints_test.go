package kritui_db

import (
	"context"
	"testing"

	"seesharpsi/kritui/llm"
)

func TestModelEndpointTypeRoundTrip(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := context.Background()

	endpointType, err := GetModelEndpointType(ctx, database, 0, "model-a")
	if err != nil {
		t.Fatalf("GetModelEndpointType() unset error: %v", err)
	}
	if endpointType != "" {
		t.Errorf("unset endpoint type = %q, want empty", endpointType)
	}

	for _, want := range []llm.EndpointType{llm.EndpointMessages, llm.EndpointChatCompletions} {
		if err := SetModelEndpointType(ctx, database, 0, " model-a ", want); err != nil {
			t.Fatalf("SetModelEndpointType(%q) error: %v", want, err)
		}
		got, err := GetModelEndpointType(ctx, database, 0, "model-a")
		if err != nil {
			t.Fatalf("GetModelEndpointType() error: %v", err)
		}
		if got != want {
			t.Errorf("endpoint type = %q, want %q", got, want)
		}
	}
}

func TestModelEndpointTypeIsScopedByProvider(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := context.Background()

	if err := SetModelEndpointType(ctx, database, 0, "model-a", llm.EndpointResponses); err != nil {
		t.Fatalf("SetModelEndpointType(provider 0) error: %v", err)
	}
	if err := SetModelEndpointType(ctx, database, EnvProviderID, "model-a", llm.EndpointMessages); err != nil {
		t.Fatalf("SetModelEndpointType(environment) error: %v", err)
	}
	if err := SetModelEndpointType(ctx, database, 7, "model-a", llm.EndpointChatCompletions); err != nil {
		t.Fatalf("SetModelEndpointType(provider 7) error: %v", err)
	}

	for providerID, want := range map[int64]llm.EndpointType{
		0:             llm.EndpointResponses,
		EnvProviderID: llm.EndpointMessages,
		7:             llm.EndpointChatCompletions,
	} {
		got, err := GetModelEndpointType(ctx, database, providerID, "model-a")
		if err != nil {
			t.Fatalf("GetModelEndpointType(%d) error: %v", providerID, err)
		}
		if got != want {
			t.Errorf("endpoint type for provider %d = %q, want %q", providerID, got, want)
		}
	}
}

func TestModelEndpointTypeRejectsInvalidValues(t *testing.T) {
	database := openMessagesTestDatabase(t, "")
	ctx := context.Background()

	if err := SetModelEndpointType(ctx, database, 0, "", llm.EndpointResponses); err == nil {
		t.Error("SetModelEndpointType() accepted empty model")
	}
	if err := SetModelEndpointType(ctx, database, 0, "model", llm.EndpointType("invalid")); err == nil {
		t.Error("SetModelEndpointType() accepted invalid endpoint type")
	}
	if _, err := database.Exec(`INSERT INTO model_endpoint_preferences (model, endpoint_type) VALUES ('model', 'invalid')`); err == nil {
		t.Error("model_endpoint_preferences accepted invalid endpoint type")
	}
}
