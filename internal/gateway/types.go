package gateway

import "errors"

var (
	ErrInvalidToken        = errors.New("gateway: invalid token")
	ErrProviderUnavailable = errors.New("gateway: provider unavailable")
)

// Binding is the token-to-provider attribution resolved for one request.
type Binding struct {
	IntegrationID  string
	AgentType      string
	Label          string
	ProviderID     string
	ProviderName   string
	ProviderKind   string
	Model          string
	CaptureMode    string
	GatewayBaseURL string
}
