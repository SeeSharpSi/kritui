package kritui_db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"seesharpsi/kritui/llm"
)

// GetModelEndpointType returns the last successful protocol for model. Its zero
// value means no protocol has succeeded yet.
func GetModelEndpointType(ctx context.Context, db *sql.DB, model string) (llm.EndpointType, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", fmt.Errorf("get model endpoint type: model is required")
	}
	var storedType string
	err := db.QueryRowContext(ctx, `
		SELECT endpoint_type
		FROM model_endpoint_preferences
		WHERE model = ?
	`, model).Scan(&storedType)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get model endpoint type: %w", err)
	}
	endpointType := llm.EndpointType(storedType)
	if !validModelEndpointType(endpointType) {
		return "", fmt.Errorf("get model endpoint type: unsupported stored type %q", storedType)
	}
	return endpointType, nil
}

// SetModelEndpointType records the most recently successful protocol for model.
func SetModelEndpointType(ctx context.Context, db *sql.DB, model string, endpointType llm.EndpointType) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return fmt.Errorf("set model endpoint type: model is required")
	}
	if !validModelEndpointType(endpointType) {
		return fmt.Errorf("set model endpoint type: unsupported type %q", endpointType)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO model_endpoint_preferences (model, endpoint_type)
		VALUES (?, ?)
		ON CONFLICT(model) DO UPDATE SET endpoint_type = excluded.endpoint_type
	`, model, endpointType); err != nil {
		return fmt.Errorf("set model endpoint type: %w", err)
	}
	return nil
}

func validModelEndpointType(endpointType llm.EndpointType) bool {
	switch endpointType {
	case llm.EndpointResponses, llm.EndpointMessages, llm.EndpointChatCompletions:
		return true
	default:
		return false
	}
}
