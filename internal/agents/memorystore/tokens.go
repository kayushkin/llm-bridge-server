package memorystore

// EstimateTokens provides a token count approximation.
// Uses ~3 characters per token for general text, which accounts for:
// - English text averages ~3.5 chars/token with Claude's tokenizer
// - Code tends to be ~3 chars/token (more symbols, short identifiers)
// - JSON structure overhead in API calls isn't counted here but is
//   accounted for by EstimateMessageOverhead and EstimateToolSchemaTokens
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	// Base estimate: ~3 chars per token
	tokens := (len(text) + 2) / 3
	return tokens
}

// EstimateMessageOverhead returns the approximate token overhead for a single
// message in the Anthropic API (role markers, JSON framing, content blocks).
// Each message adds roughly 10-15 tokens of structure.
func EstimateMessageOverhead() int {
	return 12
}

// EstimateToolSchemaTokens estimates tokens used by a tool's JSON schema
// definition. Tool schemas are sent with every request and can be significant.
// A typical tool with 3-5 parameters uses ~100-200 tokens.
func EstimateToolSchemaTokens(name, description string, paramCount int) int {
	// Base overhead for tool JSON structure
	base := 30
	// Name and description
	base += EstimateTokens(name) + EstimateTokens(description)
	// Each parameter adds ~25-40 tokens (name, type, description, required)
	base += paramCount * 35
	return base
}