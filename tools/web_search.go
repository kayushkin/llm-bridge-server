package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/kayushkin/tool-store/schema"
)

// braveSearchBaseURL is the live Brave Search API endpoint. searchBrave takes
// the base URL as a parameter rather than reading this constant directly so a
// test can point the search at a local server; nothing configures it at run
// time, because the vendor's endpoint does not vary by host.
const braveSearchBaseURL = "https://api.search.brave.com"

const (
	defaultResultCount = 5
	maxResultCount     = 10
	defaultCountry     = "US"
)

// WebSearch returns a tool that searches the web using the Brave Search API
// with braveAPIKey. The tool does not read the environment: whoever builds it
// supplies the key. With an empty key every search answers that the key is
// not set, naming the variable an operator would set.
func WebSearch(braveAPIKey string) Impl {
	type input struct {
		Query   string `json:"query"`
		Count   int    `json:"count"`
		Country string `json:"country"`
	}
	return Impl{
		Name:        "web_search",
		Description: "Search the web using Brave Search API. Returns titles, URLs, and snippets.",
		InputSchema: schema.Props([]string{"query"}, map[string]any{
			"query":   schema.Str("Search query string"),
			"count":   schema.Integer("Number of results (1-10, default 5)"),
			"country": schema.Str("2-letter country code (default US)"),
		}),
		Run: func(ctx context.Context, raw string) (string, error) {
			in, err := schema.Parse[input](raw)
			if err != nil {
				return "", err
			}

			apiKey := braveAPIKey
			if apiKey == "" {
				return "error: BRAVE_API_KEY environment variable not set", nil
			}

			count, country := applySearchParameterDefaults(in.Count, in.Country)
			return searchBrave(ctx, braveSearchBaseURL, apiKey, in.Query, count, country), nil
		},
	}
}

// applySearchParameterDefaults fills in the count and country the schema
// advertises as optional, and clamps the count to the 1-10 range the schema
// promises the caller.
func applySearchParameterDefaults(count int, country string) (int, string) {
	if count <= 0 {
		count = defaultResultCount
	}
	if count > maxResultCount {
		count = maxResultCount
	}
	if country == "" {
		country = defaultCountry
	}
	return count, country
}

// searchBrave runs one search and renders the result for the model. Transport
// and API failures are returned as readable text rather than an error, because
// that is the whole contract this tool's Run has with its caller.
func searchBrave(ctx context.Context, baseURL, apiKey, query string, count int, country string) string {
	params := url.Values{}
	params.Set("q", query)
	params.Set("count", fmt.Sprintf("%d", count))
	params.Set("country", country)

	req, err := http.NewRequestWithContext(ctx, "GET",
		baseURL+"/res/v1/web/search?"+params.Encode(), nil)
	if err != nil {
		return fmt.Sprintf("error: %s", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", apiKey)

	// Accept-Encoding is deliberately NOT set here. net/http decompresses a
	// gzipped response only when its own transport added that header; a
	// request that sets it by hand is taken to mean the caller will do the
	// decoding, so the body arrives still compressed and every read below
	// sees the gzip magic number instead of JSON. This tool asked Brave for
	// gzip and then never decoded it, and Brave does honor the request —
	// measured against the live API 2026-08-14, which means every search this
	// tool ran came back "error parsing response". Leave the header alone and
	// the transport negotiates and decodes it for us.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Sprintf("error: %s", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Sprintf("error reading response: %s", err)
	}

	if resp.StatusCode != 200 {
		return fmt.Sprintf("error: Brave API returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Sprintf("error parsing response: %s", err)
	}

	var sb strings.Builder
	for i, r := range result.Web.Results {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		fmt.Fprintf(&sb, "%d. %s\n   %s\n   %s", i+1, r.Title, r.URL, r.Description)
	}
	if sb.Len() == 0 {
		return "no results found"
	}
	return sb.String()
}
