package templates

// ProviderCard is one editable model provider in account settings. ReadOnly
// cards come from process environment or legacy storage and cannot be edited.
type ProviderCard struct {
	ID               int64
	Name             string
	Endpoint         string
	APIKeyConfigured bool
	ReadOnly         bool
}
