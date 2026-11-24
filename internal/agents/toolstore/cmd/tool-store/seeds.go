package main

import (
	"errors"
	"fmt"

	toolstore "github.com/kayushkin/tool-store"
)

// seedMCPTools upserts a curated list of well-known MCP servers as kind=mcp
// rows. New rows default to enabled=false; updates preserve the user's enabled
// state. Idempotent on every restart — descriptions and launcher specs always
// refresh from this file (which is the canonical source for these entries).
func seedMCPTools(store *toolstore.Store) error {
	seeds := []toolstore.Tool{
		{
			Name:        "brave-search",
			DisplayName: "Brave Search",
			Description: "Web search via Brave Search API. Stdio MCP server published as @modelcontextprotocol/server-brave-search.",
			Kind:        toolstore.KindMCP,
			Tags:        []string{"search", "web"},
			EnvKeys:     []string{"BRAVE_API_KEY"},
			Credentials: map[string]string{"BRAVE_API_KEY": "brave"},
			MCP: &toolstore.MCPSpec{
				Transport: "stdio",
				Command:   "npx",
				Args:      []string{"-y", "@modelcontextprotocol/server-brave-search"},
			},
		},
		{
			Name:        "playwright",
			DisplayName: "Playwright",
			Description: "Browser automation via Microsoft's Playwright MCP server (acting: navigate/click/fill/snapshot, E2E, cross-browser). Stdio, headless.",
			Kind:        toolstore.KindMCP,
			Tags:        []string{"browser", "web", "frontend", "e2e"},
			MCP: &toolstore.MCPSpec{
				Transport: "stdio",
				Command:   "npx",
				// --headless: hosts here are display-less. --browser chromium: use the cached build.
				Args: []string{"-y", "@playwright/mcp@latest", "--headless", "--browser", "chromium"},
			},
		},
		{
			Name:        "chrome-devtools",
			DisplayName: "Chrome DevTools",
			Description: "Chrome DevTools MCP server (inspecting/debugging: performance traces & Web Vitals, console, network, CPU/network emulation). Stdio, headless, drives system Google Chrome.",
			Kind:        toolstore.KindMCP,
			Tags:        []string{"browser", "web", "frontend", "performance", "debug"},
			MCP: &toolstore.MCPSpec{
				Transport: "stdio",
				Command:   "npx",
				Args:      []string{"-y", "chrome-devtools-mcp@latest", "--headless"},
			},
		},

		// Needs no credential. Each of these answered initialize and tools/list
		// from this host on 2026-09-11.
		{
			Name:        "context7",
			DisplayName: "Context7",
			Description: "Current documentation and code examples for libraries and frameworks (Upstash Context7): resolve a library id, then query its docs. Stdio, published as @upstash/context7-mcp. Runs without a key at a lower rate limit.",
			Kind:        toolstore.KindMCP,
			Tags:        []string{"docs", "libraries", "code"},
			MCP: &toolstore.MCPSpec{
				Transport: "stdio",
				Command:   "npx",
				Args:      []string{"-y", "@upstash/context7-mcp"},
			},
		},
		{
			Name:        "deepwiki",
			DisplayName: "DeepWiki",
			Description: "Generated documentation for public GitHub repositories, and questions answered about them (Cognition DeepWiki). Remote streamable-HTTP server, no auth.",
			Kind:        toolstore.KindMCP,
			Tags:        []string{"docs", "github", "code"},
			MCP: &toolstore.MCPSpec{
				Transport: "http",
				URL:       "https://mcp.deepwiki.com/mcp",
			},
		},
		{
			Name:        "fetch",
			DisplayName: "Fetch",
			Description: "Fetches a URL and returns it as markdown, in chunks for long pages. The MCP reference server, run from the official mcp/fetch Docker image because uvx is not on the harness PATH here.",
			Kind:        toolstore.KindMCP,
			Tags:        []string{"web", "fetch"},
			MCP: &toolstore.MCPSpec{
				Transport: "stdio",
				Command:   "docker",
				Args:      []string{"run", "-i", "--rm", "mcp/fetch"},
			},
		},
		{
			Name:        "time",
			DisplayName: "Time",
			Description: "Current time in any IANA zone and conversion between zones. The MCP reference server, run from the official mcp/time Docker image; the container's own zone is UTC, so name the zone in each call.",
			Kind:        toolstore.KindMCP,
			Tags:        []string{"time", "timezone"},
			MCP: &toolstore.MCPSpec{
				Transport: "stdio",
				Command:   "docker",
				Args:      []string{"run", "-i", "--rm", "mcp/time"},
			},
		},
		{
			Name:        "knowledge-graph-memory",
			DisplayName: "Knowledge Graph Memory",
			Description: "Entities, relations and observations kept in a local JSON knowledge graph (the MCP reference memory server, @modelcontextprotocol/server-memory). Not memory-store. The graph file lives inside the npx package cache, so clearing that cache erases it.",
			Kind:        toolstore.KindMCP,
			Tags:        []string{"memory", "knowledge-graph"},
			MCP: &toolstore.MCPSpec{
				Transport: "stdio",
				Command:   "npx",
				Args:      []string{"-y", "@modelcontextprotocol/server-memory"},
			},
		},
		{
			Name:        "sequential-thinking",
			DisplayName: "Sequential Thinking",
			Description: "One tool that lets a model write out numbered, revisable reasoning steps and branch them (the MCP reference server, @modelcontextprotocol/server-sequential-thinking).",
			Kind:        toolstore.KindMCP,
			Tags:        []string{"reasoning", "planning"},
			MCP: &toolstore.MCPSpec{
				Transport: "stdio",
				Command:   "npx",
				Args:      []string{"-y", "@modelcontextprotocol/server-sequential-thinking"},
			},
		},

		// Each needs a credential from auth-store under the provider named in
		// Credentials. None of those providers held a credential on 2026-09-11,
		// so provisioning one of these fails, naming the provider, until
		// someone adds it. Each started and listed its tools when given a dummy
		// key.
		{
			Name:        "github",
			DisplayName: "GitHub",
			Description: "GitHub's official MCP server: repositories, issues, pull requests, reviews, branches, files, Actions and code search. Run from the ghcr.io/github/github-mcp-server Docker image with a personal access token; the token's scopes decide what it can change.",
			Kind:        toolstore.KindMCP,
			Tags:        []string{"github", "code", "git", "issues"},
			EnvKeys:     []string{"GITHUB_PERSONAL_ACCESS_TOKEN"},
			Credentials: map[string]string{"GITHUB_PERSONAL_ACCESS_TOKEN": "github"},
			MCP: &toolstore.MCPSpec{
				Transport: "stdio",
				Command:   "docker",
				// -e with no value forwards the variable from the provisioned env.
				Args: []string{"run", "-i", "--rm", "-e", "GITHUB_PERSONAL_ACCESS_TOKEN", "ghcr.io/github/github-mcp-server"},
			},
		},
		{
			Name:        "sentry",
			DisplayName: "Sentry",
			Description: "Sentry's official MCP server: find organizations and projects, search issues and events, update issues, and run Seer analysis. Stdio, published as @sentry/mcp-server, authenticated by a Sentry user auth token.",
			Kind:        toolstore.KindMCP,
			Tags:        []string{"monitoring", "errors", "debug"},
			EnvKeys:     []string{"SENTRY_ACCESS_TOKEN"},
			Credentials: map[string]string{"SENTRY_ACCESS_TOKEN": "sentry"},
			MCP: &toolstore.MCPSpec{
				Transport: "stdio",
				Command:   "npx",
				Args:      []string{"-y", "@sentry/mcp-server"},
			},
		},
		{
			Name:        "notion",
			DisplayName: "Notion",
			Description: "Notion's official MCP server over the Notion API: search, read and write pages, blocks, databases, comments and users. Stdio, published as @notionhq/notion-mcp-server, authenticated by an integration token that sees only the pages shared with it.",
			Kind:        toolstore.KindMCP,
			Tags:        []string{"notes", "docs", "productivity"},
			EnvKeys:     []string{"NOTION_TOKEN"},
			Credentials: map[string]string{"NOTION_TOKEN": "notion"},
			MCP: &toolstore.MCPSpec{
				Transport: "stdio",
				Command:   "npx",
				Args:      []string{"-y", "@notionhq/notion-mcp-server"},
			},
		},
		{
			Name:        "firecrawl",
			DisplayName: "Firecrawl",
			Description: "Scrape, crawl, map and search the web through the Firecrawl API, returning clean markdown or structured extracts, including JavaScript-rendered pages. Stdio, published as firecrawl-mcp.",
			Kind:        toolstore.KindMCP,
			Tags:        []string{"web", "scraping", "search"},
			EnvKeys:     []string{"FIRECRAWL_API_KEY"},
			Credentials: map[string]string{"FIRECRAWL_API_KEY": "firecrawl"},
			MCP: &toolstore.MCPSpec{
				Transport: "stdio",
				Command:   "npx",
				Args:      []string{"-y", "firecrawl-mcp"},
			},
		},
		{
			Name:        "exa",
			DisplayName: "Exa Search",
			Description: "Neural web search and page fetch through the Exa API. Stdio, published as exa-mcp-server. Exa also hosts an OAuth server at https://mcp.exa.ai/mcp; this row uses an API key instead, so an unattended session can use it.",
			Kind:        toolstore.KindMCP,
			Tags:        []string{"search", "web"},
			EnvKeys:     []string{"EXA_API_KEY"},
			Credentials: map[string]string{"EXA_API_KEY": "exa"},
			MCP: &toolstore.MCPSpec{
				Transport: "stdio",
				Command:   "npx",
				Args:      []string{"-y", "exa-mcp-server"},
			},
		},
		{
			Name:        "supabase",
			DisplayName: "Supabase",
			Description: "Supabase's MCP server: organizations, projects, SQL, tables, migrations, logs, edge functions and docs search. Stdio, published as @supabase/mcp-server-supabase, authenticated by a personal access token, and started --read-only so it cannot write; remove that flag here to allow writes.",
			Kind:        toolstore.KindMCP,
			Tags:        []string{"database", "postgres", "backend"},
			EnvKeys:     []string{"SUPABASE_ACCESS_TOKEN"},
			Credentials: map[string]string{"SUPABASE_ACCESS_TOKEN": "supabase"},
			MCP: &toolstore.MCPSpec{
				Transport: "stdio",
				Command:   "npx",
				Args:      []string{"-y", "@supabase/mcp-server-supabase", "--read-only"},
			},
		},
	}

	for _, t := range seeds {
		enabled := false
		if existing, err := store.GetToolByName(t.Name); err == nil {
			enabled = existing.Enabled
		} else if !errors.Is(err, toolstore.ErrNotFound) {
			return fmt.Errorf("lookup mcp tool %s: %w", t.Name, err)
		}
		t.Enabled = enabled
		if _, err := store.UpsertTool(&t); err != nil {
			return fmt.Errorf("upsert mcp tool %s: %w", t.Name, err)
		}
	}
	return nil
}
