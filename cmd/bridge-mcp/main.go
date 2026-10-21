// bridge-mcp is an MCP server, over stdio, that gives an agent the bridge's
// session features as tools: make an image, show the person a file, and offer
// a button in the chat.
//
// It acts for the one session that started it, with the posting token
// llm-bridge-server gives every harness child (LLM_BRIDGE_SESSION_POSTING_TOKEN
// and the LLM_BRIDGE_SESSION_*_URL addresses beside it). A process that was
// not started by the server has none of them, and every tool says so rather
// than guessing.
//
// Each tool is one call to a server route, and the server's answer comes back
// unchanged, so the routes stay the one place a rule lives. bridge-share-file
// and bridge-button are the same routes for a shell.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	postingTokenEnvironmentVariable = "LLM_BRIDGE_SESSION_POSTING_TOKEN"
	imagesURLEnvironmentVariable    = "LLM_BRIDGE_SESSION_IMAGES_URL"
	filesURLEnvironmentVariable     = "LLM_BRIDGE_SESSION_FILES_URL"
	actionsURLEnvironmentVariable   = "LLM_BRIDGE_SESSION_ACTIONS_URL"
)

// previewMaximumSide is the longest side, in pixels, of the copy of each
// generated image the tool hands back for the model to look at. The image the
// person sees in the chat is the full one.
const previewMaximumSide = 1024

// imageGenerationTimeout is the server's own limit on one call plus a margin
// for sharing the images.
const imageGenerationTimeout = 7 * time.Minute

type generateImageInput struct {
	Prompt              string   `json:"prompt" jsonschema:"what to draw: subject, style, composition, colours, any text to letter in it"`
	ReferenceImagePaths []string `json:"reference_image_paths,omitempty" jsonschema:"absolute paths of images on this host to edit, or to take a character, style or layout from"`
}

type shareFileInput struct {
	Paths []string `json:"paths" jsonschema:"absolute paths of the files to show the person; an image is drawn in the chat and a video played"`
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("bridge-mcp: ")
	server := mcp.NewServer(&mcp.Implementation{Name: "bridge", Version: "1"}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name: "generate_image",
		Description: "Make images with an image model from a prompt, and optionally from reference images to edit or follow. " +
			"Each image is shown to the person in this chat and saved on this host; the answer gives the saved paths and a preview of each for you to look at. " +
			"One call takes 30 to 90 seconds. Use it for pictures a model should paint (illustrations, characters, textures, concept art, photos); " +
			"draw with code instead when the picture must be exact, editable or animated (charts, diagrams, UI, p5 or three.js scenes).",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPointer(false), OpenWorldHint: boolPointer(true)},
	}, generateImage)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "share_file",
		Description: "Show files to the person in this chat, at this point in the conversation. An image is drawn and a video played. Answers with each file's record.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPointer(false)},
	}, shareFile)

	mcp.AddTool(server, &mcp.Tool{
		Name: "offer_button",
		Description: "Put a button in this chat that runs one step when the person presses it, and shows the result under it. Never a way to ask a question. " +
			"type is one of deploy (repo_id), run_scheduler_job (scheduler_job_id), run_command (shell_command; optional working_directory, result_format text|markdown, inputs), " +
			"model_call (message, model, maximum_cost_usd), background_agent (message, maximum_cost_usd; optional model), fork_and_send (message), new_session_and_send (message), " +
			"send_to_session (target_session_id, message; optional inputs) or stop_session (target_session_id). " +
			"A reviewer model reads every run_command. A failed press is always reported to you as a new message; a successful one only when report_success is true, " +
			"which it is by default for run_command and run_scheduler_job: set it false when the result under the button is all the person needs, true when your next step waits on a press of another kind. " +
			"Write the action_id from the answer in your reply where the button should appear.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPointer(false)},
	}, offerButton)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}

func generateImage(ctx context.Context, _ *mcp.CallToolRequest, input generateImageInput) (*mcp.CallToolResult, msg.ImageGenerationResponse, error) {
	var response msg.ImageGenerationResponse
	imagesURL, token, err := sessionAddress(imagesURLEnvironmentVariable)
	if err != nil {
		return nil, response, err
	}
	body, err := json.Marshal(msg.ImageGenerationRequest{Prompt: input.Prompt, ReferenceImagePaths: input.ReferenceImagePaths})
	if err != nil {
		return nil, response, err
	}
	callContext, cancel := context.WithTimeout(ctx, imageGenerationTimeout)
	defer cancel()
	answer, err := postToSession(callContext, imagesURL, token, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, response, err
	}
	if err := json.Unmarshal(answer, &response); err != nil {
		return nil, response, fmt.Errorf("the server's answer is not an image generation response: %v (%s)", err, answer)
	}

	result := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(answer)}}}
	for _, generated := range response.Images {
		preview, err := previewOfImage(generated.Path, previewMaximumSide)
		if err != nil {
			// The images were made and shown; only the model's own look is
			// missing, and it is told so.
			result.Content = append(result.Content, &mcp.TextContent{Text: fmt.Sprintf("no preview of %s: %v; read the file to look at it", generated.Path, err)})
			continue
		}
		result.Content = append(result.Content, &mcp.ImageContent{Data: preview, MIMEType: "image/jpeg"})
	}
	return result, response, nil
}

// sharedFiles is share_file's answer: each file's record as the server
// answered it. A record is decoded to a map, not kept as json.RawMessage,
// because the SDK describes a RawMessage's schema as an array of bytes and
// then refuses the object it holds.
type sharedFiles struct {
	SessionFiles []map[string]any `json:"session_files"`
}

func shareFile(ctx context.Context, _ *mcp.CallToolRequest, input shareFileInput) (*mcp.CallToolResult, sharedFiles, error) {
	var output sharedFiles
	filesURL, token, err := sessionAddress(filesURLEnvironmentVariable)
	if err != nil {
		return nil, output, err
	}
	if len(input.Paths) == 0 {
		return nil, output, errors.New("paths is empty")
	}
	for _, path := range input.Paths {
		record, err := shareOneFile(ctx, filesURL, token, path)
		if err != nil {
			return nil, output, fmt.Errorf("%s: %w (files before it were shared: %d)", path, err, len(output.SessionFiles))
		}
		output.SessionFiles = append(output.SessionFiles, record)
	}
	return nil, output, nil
}

func shareOneFile(ctx context.Context, filesURL, token, path string) (map[string]any, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	head := make([]byte, 512)
	read, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	mediaType := mime.TypeByExtension(filepath.Ext(path))
	if mediaType == "" {
		mediaType = http.DetectContentType(head[:read])
	}
	address, err := url.Parse(filesURL)
	if err != nil {
		return nil, fmt.Errorf("%s is not a URL: %w", filesURLEnvironmentVariable, err)
	}
	query := address.Query()
	query.Set("filename", filepath.Base(path))
	address.RawQuery = query.Encode()
	answer, err := postToSession(ctx, address.String(), token, mediaType, file)
	if err != nil {
		return nil, err
	}
	return decodeRecord(answer)
}

// decodeRecord decodes one JSON object the server answered with.
func decodeRecord(answer []byte) (map[string]any, error) {
	var record map[string]any
	if err := json.Unmarshal(answer, &record); err != nil {
		return nil, fmt.Errorf("the server's answer is not a JSON object: %v (%s)", err, answer)
	}
	return record, nil
}

func offerButton(ctx context.Context, _ *mcp.CallToolRequest, offer msg.SessionActionOffer) (*mcp.CallToolResult, map[string]any, error) {
	actionsURL, token, err := sessionAddress(actionsURLEnvironmentVariable)
	if err != nil {
		return nil, nil, err
	}
	body, err := json.Marshal(offer)
	if err != nil {
		return nil, nil, err
	}
	answer, err := postToSession(ctx, actionsURL, token, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	record, err := decodeRecord(answer)
	return nil, record, err
}

// sessionAddress reads one of the addresses the server gave this process, and
// the posting token.
func sessionAddress(urlEnvironmentVariable string) (string, string, error) {
	address := os.Getenv(urlEnvironmentVariable)
	token := os.Getenv(postingTokenEnvironmentVariable)
	switch {
	case token == "":
		return "", "", fmt.Errorf("%s is unset: this agent was not started by llm-bridge-server, so it has no session to post into", postingTokenEnvironmentVariable)
	case address == "":
		return "", "", fmt.Errorf("%s is unset: llm-bridge-server started this agent without that feature (file-store is not wired to it)", urlEnvironmentVariable)
	}
	return address, token, nil
}

// postToSession posts to one of the session's routes and returns the answer's
// body, or an error carrying it when the status is not 2xx.
func postToSession(ctx context.Context, address, token, contentType string, body io.Reader) (json.RawMessage, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", contentType)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read the server's answer: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, fmt.Errorf("llm-bridge-server answered %d: %s", response.StatusCode, answer)
	}
	return answer, nil
}

// previewOfImage decodes an image and returns it as a JPEG whose longest side
// is at most maximumSide, each output pixel the average of the source pixels
// it covers.
func previewOfImage(path string, maximumSide int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	source, _, err := image.Decode(file)
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width == 0 || height == 0 {
		return nil, errors.New("the image is empty")
	}
	scale := 1.0
	if longest := max(width, height); longest > maximumSide {
		scale = float64(maximumSide) / float64(longest)
	}
	previewWidth := max(1, int(float64(width)*scale))
	previewHeight := max(1, int(float64(height)*scale))
	preview := image.NewRGBA(image.Rect(0, 0, previewWidth, previewHeight))
	for previewY := 0; previewY < previewHeight; previewY++ {
		sourceTop := bounds.Min.Y + previewY*height/previewHeight
		sourceBottom := max(sourceTop+1, bounds.Min.Y+(previewY+1)*height/previewHeight)
		for previewX := 0; previewX < previewWidth; previewX++ {
			sourceLeft := bounds.Min.X + previewX*width/previewWidth
			sourceRight := max(sourceLeft+1, bounds.Min.X+(previewX+1)*width/previewWidth)
			var red, green, blue, alpha, count uint64
			for y := sourceTop; y < sourceBottom; y++ {
				for x := sourceLeft; x < sourceRight; x++ {
					r, g, b, a := source.At(x, y).RGBA()
					red, green, blue, alpha, count = red+uint64(r), green+uint64(g), blue+uint64(b), alpha+uint64(a), count+1
				}
			}
			preview.Set(previewX, previewY, color.RGBA64{
				R: uint16(red / count), G: uint16(green / count), B: uint16(blue / count), A: uint16(alpha / count),
			})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, preview, &jpeg.Options{Quality: 85}); err != nil {
		return nil, fmt.Errorf("encode the preview: %w", err)
	}
	return encoded.Bytes(), nil
}

func boolPointer(value bool) *bool { return &value }
