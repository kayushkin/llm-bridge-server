package aiauth

// Provider defines the interface for an LLM auth provider.
type Provider interface {
	ID() string
	Login(callbacks LoginCallbacks) (*Credential, error)
	RefreshToken(cred *Credential) (*Credential, error)
}

// LoginCallbacks provides hooks for interactive login flows.
type LoginCallbacks struct {
	OnAuthURL func(url string) error              // open browser
	OnPrompt  func(message string) (string, error) // get user input
}
