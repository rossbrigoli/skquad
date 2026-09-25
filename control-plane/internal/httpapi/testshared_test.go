// Shared test fixtures: string constants for URLs, auth headers and
// repeated literals (S-126 / S1192). Kept in one file so every
// httpapi test file shares a single source of truth.

package httpapi

const (
	aliceEmail                = "alice@example.com"
	authAdmin                 = "Bearer admin"
	authAlice                 = "Bearer alice"
	authOwner                 = "Bearer owner"
	authViewer                = "Bearer viewer"
	cascadeKeyConvergedAction = "aimodel.cascade.key_converged"
	deadProviderName          = "Dead Provider"
	forceQuery                = "?force=true"
	legacyFieldValue          = "ignored-legacy-field"
	localFastModel            = "openai/local-fast"
	localLLMBaseURL           = "http://localhost:8123/v1"
	modelOne                  = "openai/one"
	modelTwo                  = "openai/two"
	pathAIModelItem           = "/api/v1/ai-models/"
	pathAIModels              = "/api/v1/ai-models"
	pathAgentsPrefix          = "/api/v1/agents/"
	pathAuthMe                = "/api/v1/auth/me"
	pathDashboard             = "/api/v1/dashboard"
	pathDeprecate             = "/deprecate"
	pathIdentity              = "/identity"
	pathModels                = "/models"
	pathModelsSlash           = "/models/"
	pathMyModels              = "/api/v1/models/me"
	pathPermissions           = "/permissions"
	pathProvidersPrefix       = "/api/v1/registry/llm-providers/"
	pathSquadsPrefix          = "/api/v1/squads/"
	pathUsersPrefix           = "/api/v1/users/"
	sharedModelName           = "shared-name"
	statusInProgress          = "in-progress"
	testIssuer                = "https://issuer.example.com"
)
