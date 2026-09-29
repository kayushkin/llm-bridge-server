package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"mvdan.cc/sh/v3/syntax"

	"github.com/kayushkin/llm-bridge-server/internal/harness"
)

// Files named by a session's tools (`GET /sessions/{id}/files-named-by-tools`).
//
// An agent's reply names files the way a person would — `sessions.go:707`,
// `internal/server/renamer.go` — relative to wherever it happened to be
// looking. The chat turns such a mention into a chip that opens the file, and
// to do that it needs the absolute path. This is where the path comes from:
// every file the session's own tools named, either in a call (Read's
// `file_path`, a path in a Bash command resolved against the `cd` before it)
// or in a result (grep, find and Glob output, resolved against the directory
// the command ran in). The chat matches a mention against this list; it never
// guesses a path the session did not name.
//
// The same list bounds the content route: it serves a file only if it is on
// the list, so the route cannot be used to read an arbitrary file on the host.
// A path the session's bundle denies reading is left off.

// maximumFileNamedByToolsBytes is the largest file the content route returns.
const maximumFileNamedByToolsBytes = 1 << 20

// maximumOutputLinesScannedPerToolResult bounds the scan of one result, so a
// `find /` that printed a million lines does not stall the request.
const maximumOutputLinesScannedPerToolResult = 5000

// maximumFilesNamedByToolsCandidates bounds how many distinct paths are
// checked on disk for one session.
const maximumFilesNamedByToolsCandidates = 20000

// FileNamedByTools is one existing regular file a session's tools named.
type FileNamedByTools struct {
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
}

// FileNamedByToolsContent is one such file's current content on disk.
type FileNamedByToolsContent struct {
	FileNamedByTools
	Content string `json:"content"`
}

// handleFilesNamedByTools lists the files the session's tools named that
// exist on disk now.
func (s *Server) handleFilesNamedByTools(w http.ResponseWriter, r *http.Request) {
	files, ok := s.filesNamedByToolsOrWriteError(w, r.PathValue("id"))
	if !ok {
		return
	}
	writeJSON(w, map[string]any{"files": files})
}

// handleFileNamedByToolsContent returns one file's current content, but only
// for a path on the session's list.
func (s *Server) handleFileNamedByToolsContent(w http.ResponseWriter, r *http.Request) {
	requested := r.URL.Query().Get("path")
	if requested == "" || !filepath.IsAbs(requested) {
		writeJSONError(w, http.StatusBadRequest, "path_not_absolute", "path must be an absolute path from files-named-by-tools")
		return
	}
	files, ok := s.filesNamedByToolsOrWriteError(w, r.PathValue("id"))
	if !ok {
		return
	}
	var found *FileNamedByTools
	for i := range files {
		if files[i].Path == requested {
			found = &files[i]
			break
		}
	}
	if found == nil {
		writeJSONError(w, http.StatusNotFound, "file_not_named_by_session_tools", "no tool in this session named that file, or it no longer exists")
		return
	}
	if found.Size > maximumFileNamedByToolsBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "file_too_large",
			fmt.Sprintf("the file is %d bytes; this route returns at most %d", found.Size, maximumFileNamedByToolsBytes))
		return
	}
	content, err := os.ReadFile(found.Path)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "read_file", err.Error())
		return
	}
	if !utf8.Valid(content) || strings.ContainsRune(string(content), 0) {
		writeJSONError(w, http.StatusUnsupportedMediaType, "file_not_text", "the file is not UTF-8 text")
		return
	}
	writeJSON(w, FileNamedByToolsContent{FileNamedByTools: *found, Content: string(content)})
}

func (s *Server) filesNamedByToolsOrWriteError(w http.ResponseWriter, sessionID string) ([]FileNamedByTools, bool) {
	session, err := s.store.GetSession(sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "session not found", http.StatusNotFound)
		return nil, false
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "read_session", err.Error())
		return nil, false
	}
	deniedReadPaths, err := sessionDeniedReadPaths(session)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "read_denied_read_paths", err.Error())
		return nil, false
	}
	events, err := s.harness.ListToolEventsInOrder(session.SessionID)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "list_tool_events", err.Error())
		return nil, false
	}
	homeDirectory, err := os.UserHomeDir()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "home_directory", err.Error())
		return nil, false
	}
	workingDirectory := ""
	if session.Info != nil {
		workingDirectory = session.Info.WorkingDir
	}
	candidates := pathsNamedByToolEvents(events, workingDirectory, homeDirectory)
	files := make([]FileNamedByTools, 0, len(candidates))
	for _, candidate := range candidates {
		if pathIsDenied(candidate, deniedReadPaths, homeDirectory) {
			continue
		}
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, FileNamedByTools{Path: candidate, Size: info.Size(), ModifiedAt: info.ModTime().UTC()})
	}
	return files, true
}

// pathsNamedByToolEvents walks the session's tool events in order and returns
// every absolute path they name, first mention first, without looking at the
// disk. Shell commands move a working directory that persists from one call
// to the next, as Claude Code's Bash tool does; a call's `workdir` (codex)
// sets it for that call.
func pathsNamedByToolEvents(events []harness.ToolEventInOrder, workingDirectory, homeDirectory string) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(candidate, directory string) {
		if len(out) >= maximumFilesNamedByToolsCandidates {
			return
		}
		absolute := absolutePathOf(candidate, directory, homeDirectory)
		if absolute == "" {
			return
		}
		if _, ok := seen[absolute]; ok {
			return
		}
		seen[absolute] = struct{}{}
		out = append(out, absolute)
	}

	currentDirectory := workingDirectory
	directoryOfCall := make(map[string]string)
	for _, event := range events {
		if call := event.Call; call != nil {
			var input map[string]any
			if err := json.Unmarshal(call.Input, &input); err != nil {
				continue
			}
			for _, key := range []string{"file_path", "notebook_path", "path"} {
				if value, ok := input[key].(string); ok && value != "" {
					add(value, currentDirectory)
				}
			}
			callDirectory := currentDirectory
			if workdir, ok := input["workdir"].(string); ok && workdir != "" {
				callDirectory = absolutePathOf(workdir, currentDirectory, homeDirectory)
			}
			if script := shellScriptOfToolInput(input["command"]); script != "" {
				callDirectory = pathsNamedByShellScript(script, callDirectory, homeDirectory, add)
				if _, hasWorkdir := input["workdir"]; !hasWorkdir {
					currentDirectory = callDirectory
				}
			}
			if call.ToolID != "" {
				directoryOfCall[call.ToolID] = callDirectory
			}
			continue
		}
		result := event.Result
		if result == nil || result.ToolID == "" {
			continue
		}
		directory, ok := directoryOfCall[result.ToolID]
		if !ok {
			continue
		}
		for lineNumber, line := range strings.Split(result.Output, "\n") {
			if lineNumber >= maximumOutputLinesScannedPerToolResult {
				break
			}
			for _, token := range strings.Fields(line) {
				// grep prints `path:line:text`, so the path ends at the first colon.
				if colon := strings.IndexByte(token, ':'); colon > 0 {
					token = token[:colon]
				}
				token = strings.Trim(token, `"'(),;`+"`")
				if looksLikeFilePath(token) {
					add(token, directory)
				}
			}
		}
	}
	return out
}

// shellScriptOfToolInput is the script a shell tool ran: Claude Code's Bash
// sends a string, codex an argv such as ["bash","-lc","<script>"].
func shellScriptOfToolInput(command any) string {
	switch value := command.(type) {
	case string:
		return value
	case []any:
		arguments := make([]string, 0, len(value))
		for _, argument := range value {
			text, ok := argument.(string)
			if !ok {
				return ""
			}
			arguments = append(arguments, text)
		}
		if len(arguments) >= 3 && (arguments[len(arguments)-2] == "-lc" || arguments[len(arguments)-2] == "-c") {
			return arguments[len(arguments)-1]
		}
		quoted := make([]string, 0, len(arguments))
		for _, argument := range arguments {
			quotedArgument, err := syntax.Quote(argument, syntax.LangBash)
			if err != nil {
				return ""
			}
			quoted = append(quoted, quotedArgument)
		}
		return strings.Join(quoted, " ")
	}
	return ""
}

// pathsNamedByShellScript calls add for every literal word in the script that
// looks like a file path, resolved against the directory the script had
// reached at that word (`cd` moves it), and returns the directory the script
// ended in. A script that does not parse names nothing and moves nothing.
func pathsNamedByShellScript(script, directory, homeDirectory string, add func(candidate, directory string)) string {
	file, err := syntax.NewParser().Parse(strings.NewReader(script), "")
	if err != nil {
		return directory
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		switch node := node.(type) {
		case *syntax.CallExpr:
			words := make([]string, 0, len(node.Args))
			for _, word := range node.Args {
				words = append(words, literalOfShellWord(word))
			}
			if len(words) > 0 && words[0] == "cd" {
				switch {
				case len(words) == 1:
					directory = homeDirectory
				case words[1] != "" && words[1] != "-":
					directory = absolutePathOf(words[1], directory, homeDirectory)
				}
				return true
			}
			for _, word := range words {
				// `--file=path` and `-Cpath` style flags are left out on purpose:
				// the gain is small and the guesses are often wrong.
				if looksLikeFilePath(word) {
					add(word, directory)
				}
			}
		case *syntax.Redirect:
			if node.Word != nil {
				if word := literalOfShellWord(node.Word); looksLikeFilePath(word) {
					add(word, directory)
				}
			}
		}
		return true
	})
	return directory
}

// literalOfShellWord is a word's text when it is literal — plain, single- or
// double-quoted text with no expansion — and "" otherwise.
func literalOfShellWord(word *syntax.Word) string {
	var builder strings.Builder
	for _, part := range word.Parts {
		switch part := part.(type) {
		case *syntax.Lit:
			builder.WriteString(part.Value)
		case *syntax.SglQuoted:
			builder.WriteString(part.Value)
		case *syntax.DblQuoted:
			for _, inner := range part.Parts {
				literal, ok := inner.(*syntax.Lit)
				if !ok {
					return ""
				}
				builder.WriteString(literal.Value)
			}
		default:
			return ""
		}
	}
	return builder.String()
}

// bareFileNamePattern is a file name with an extension that starts with a
// lowercase letter: `sessions.go`, `Chat.module.css` — not `1.2`, `v2` or a Go
// selector such as `config.Load`.
var bareFileNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.@+-]*[A-Za-z0-9_@+-]\.[a-z][A-Za-z0-9]{0,9}$`)

// looksLikeFilePath says whether a token is worth checking on disk: a path
// with a slash, or a bare file name with an extension.
func looksLikeFilePath(token string) bool {
	if token == "" || len(token) > 4096 || strings.HasPrefix(token, "-") {
		return false
	}
	if strings.Contains(token, "://") || strings.ContainsAny(token, "*?[]{}$<>|&;= \t") {
		return false
	}
	if strings.Contains(token, "/") {
		return true
	}
	return bareFileNamePattern.MatchString(token)
}

// absolutePathOf resolves a path as a shell would: `~` to the home directory,
// a relative path against the directory given. A relative path with no known
// directory resolves to "".
func absolutePathOf(candidate, directory, homeDirectory string) string {
	switch {
	case candidate == "~":
		return homeDirectory
	case strings.HasPrefix(candidate, "~/"):
		return filepath.Join(homeDirectory, candidate[2:])
	case filepath.IsAbs(candidate):
		return filepath.Clean(candidate)
	case directory == "":
		return ""
	default:
		return filepath.Join(directory, candidate)
	}
}

// pathIsDenied says whether a bundle's denied read paths cover the file: the
// path itself, anything under it as a directory, or a match of it as a glob
// (`/secrets/**` is taken as the directory `/secrets`).
func pathIsDenied(absolutePath string, deniedReadPaths []string, homeDirectory string) bool {
	for _, denied := range deniedReadPaths {
		pattern := absolutePathOf(denied, "", homeDirectory)
		if pattern == "" {
			continue
		}
		pattern = strings.TrimSuffix(strings.TrimSuffix(pattern, "/**"), "/*")
		if absolutePath == pattern || strings.HasPrefix(absolutePath, pattern+"/") {
			return true
		}
		if matched, err := filepath.Match(pattern, absolutePath); err == nil && matched {
			return true
		}
	}
	return false
}
