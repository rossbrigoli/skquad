package httpapi

// Shared string literals (S-189, sonar S1192): single source of truth for
// header names, content types, route patterns and repeated error messages.
const (
	hdrContentType   = "Content-Type"
	hdrAccept        = "Accept"
	contentTypeJSON  = "application/json"
	bearerAuthPrefix = "Bearer "
	hdrAuthorization = "Authorization"

	msgStatusInvalid    = "status is invalid"
	msgMessageRequired  = "message is required"
	msgGatewayKeyFailed = "failed to provision LLM gateway virtual key"
	msgAppliesToInvalid = "applies_to must be squad, agent or both"

	routePromptTemplateByID = "/prompt-templates/{templateID}"
)
