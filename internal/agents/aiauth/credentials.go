package aiauth

// Credential represents authentication credentials for an LLM provider.
type Credential struct {
	Type     string `json:"type"`               // "api_key", "token", "oauth"
	Provider string `json:"provider"`
	Key      string `json:"key,omitempty"`     // for api_key
	Token    string `json:"token,omitempty"`   // for token
	Access   string `json:"access,omitempty"`  // for oauth
	Refresh  string `json:"refresh,omitempty"` // for oauth
	Expires  int64  `json:"expires,omitempty"` // unix ms for oauth
	Email    string `json:"email,omitempty"`
}

// credentialPriority returns a sort order (lower = higher priority).
func credentialPriority(c *Credential) int {
	switch c.Type {
	case "oauth":
		return 0
	case "token":
		return 1
	case "api_key":
		return 2
	default:
		return 3
	}
}
