package server

// Image generation: a prompt, and optionally reference images, in; image
// files out. One harness instance makes them (image_generation.instance_id),
// running its binary in -generate-image mode the way a one-shot call runs it
// in -oneshot mode. llm-bridge-codex implements the mode on the Codex login,
// so an image costs the ChatGPT subscription's allowance and no API key.
//
// POST /images is for operator callers and answers with the files' paths.
// POST /sessions/{id}/images is for a session's own agent: it also shares
// each image into that session, so the person sees it in the chat.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/childprocessenv"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// imageGenerationTimeout bounds one call. The harness gives codex five
// minutes; this leaves it room to say so itself.
const imageGenerationTimeout = 6 * time.Minute

// imageGenerationFailure is a refusal or failure as a route answers it.
type imageGenerationFailure struct {
	status  int
	code    string
	message string
}

func (failure *imageGenerationFailure) Error() string { return failure.code + ": " + failure.message }

// decodeImageGenerationRequest reads and checks a request body.
func decodeImageGenerationRequest(r *http.Request) (msg.ImageGenerationRequest, *imageGenerationFailure) {
	var request msg.ImageGenerationRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return request, &imageGenerationFailure{http.StatusBadRequest, "invalid_body", err.Error()}
	}
	if strings.TrimSpace(request.Prompt) == "" {
		return request, &imageGenerationFailure{http.StatusBadRequest, "prompt_required", "prompt is required"}
	}
	for _, path := range request.ReferenceImagePaths {
		if !filepath.IsAbs(path) {
			return request, &imageGenerationFailure{http.StatusBadRequest, "reference_image_path_not_absolute",
				fmt.Sprintf("reference image path %q is not absolute", path)}
		}
	}
	return request, nil
}

// generateImages runs the configured instance's harness in -generate-image
// mode and returns its answer with InstanceID set.
func (s *Server) generateImages(ctx context.Context, request msg.ImageGenerationRequest) (msg.ImageGenerationResponse, *imageGenerationFailure) {
	var response msg.ImageGenerationResponse
	instanceID := s.settings.String(config.SettingImageGenerationInstanceID)
	if instanceID == "" {
		return response, &imageGenerationFailure{http.StatusServiceUnavailable, "image_generation_off",
			config.SettingImageGenerationInstanceID + " is empty, so this server makes no images"}
	}
	instance, err := s.harnessStore.GetInstance(instanceID)
	if err != nil {
		return response, &imageGenerationFailure{http.StatusServiceUnavailable, "image_generation_instance_unavailable",
			fmt.Sprintf("%s names instance %q: %v", config.SettingImageGenerationInstanceID, instanceID, err)}
	}
	if !instance.Enabled {
		return response, &imageGenerationFailure{http.StatusServiceUnavailable, "image_generation_instance_unavailable",
			fmt.Sprintf("%s names instance %q, which is disabled", config.SettingImageGenerationInstanceID, instanceID)}
	}
	binaryPath, ok := harness.Available(instance.HarnessType)
	if !ok {
		return response, &imageGenerationFailure{http.StatusBadGateway, "harness_binary_not_found",
			"harness binary not found: " + msg.HarnessBinaryName(instance.HarnessType)}
	}

	callContext, cancel := context.WithTimeout(ctx, imageGenerationTimeout)
	defer cancel()
	body, err := json.Marshal(request)
	if err != nil {
		return response, &imageGenerationFailure{http.StatusInternalServerError, "encode_request", err.Error()}
	}
	command := exec.CommandContext(callContext, binaryPath, "-generate-image")
	command.Env = childprocessenv.EnvironmentWithoutServerSecrets()
	command.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	started := time.Now()
	runErr := command.Run()
	log.Printf("[image-generation] caller=%q instance=%s took %s, exit %v", request.Caller, instanceID, time.Since(started).Round(time.Second), runErr)
	if runErr != nil {
		var harnessError struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(stdout.Bytes(), &harnessError) == nil && harnessError.Error != "" {
			return response, &imageGenerationFailure{http.StatusBadGateway, "image_generation_failed", harnessError.Error}
		}
		return response, &imageGenerationFailure{http.StatusBadGateway, "image_generation_failed",
			fmt.Sprintf("%s -generate-image: %v (stdout: %s; stderr: %s)", binaryPath, runErr, stdout.String(), stderr.String())}
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		return response, &imageGenerationFailure{http.StatusBadGateway, "image_generation_failed",
			fmt.Sprintf("%s -generate-image answered with something that is not an image generation response: %v (%s)", binaryPath, err, stdout.String())}
	}
	if len(response.Images) == 0 {
		return response, &imageGenerationFailure{http.StatusBadGateway, "image_generation_failed",
			binaryPath + " -generate-image succeeded but listed no image"}
	}
	response.InstanceID = instanceID
	return response, nil
}

// handleGenerateImages is POST /images: make images and answer with where
// they were saved.
func (s *Server) handleGenerateImages(w http.ResponseWriter, r *http.Request) {
	request, failure := decodeImageGenerationRequest(r)
	if failure == nil {
		var response msg.ImageGenerationResponse
		response, failure = s.generateImages(r.Context(), request)
		if failure == nil {
			writeJSON(w, response)
			return
		}
	}
	writeJSONError(w, failure.status, failure.code, failure.message)
}

// handleGenerateImagesIntoSession is POST /sessions/{id}/images: make images
// and share each into the session, as its agent when the caller holds the
// session's posting token. Answers with the images and their session files.
func (s *Server) handleGenerateImagesIntoSession(w http.ResponseWriter, r *http.Request) {
	if s.fileStore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "session_files_off",
			"LLMBRIDGE_FILE_STORE_URL is not set, so this server cannot share images into a session; POST /images makes them without sharing")
		return
	}
	session, err := s.store.GetSession(r.PathValue("id"))
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	request, failure := decodeImageGenerationRequest(r)
	if failure != nil {
		writeJSONError(w, failure.status, failure.code, failure.message)
		return
	}
	if request.Caller == "" {
		request.Caller = "session " + session.SessionID
	}
	response, failure := s.generateImages(r.Context(), request)
	if failure != nil {
		writeJSONError(w, failure.status, failure.code, failure.message)
		return
	}

	sharedBy := msg.SessionFileSharedByUser
	if caller, ok := callerOfRequest(r); ok && caller.postingAgentOfSessionID != "" {
		sharedBy = msg.SessionFileSharedByAgent
	}
	limits, err := s.fileStore.Limits()
	if err != nil {
		writeFileStoreError(w, err)
		return
	}
	for _, image := range response.Images {
		record, shareFailure := s.shareGeneratedImageIntoSession(session, image, limits.MaximumFileBytes, sharedBy)
		if shareFailure != nil {
			// The images exist; the caller is told where, so none is lost to a
			// failed share.
			shareFailure.message = fmt.Sprintf("%s (the images were made and are at %s)", shareFailure.message, generatedImagePaths(response.Images))
			if shareFailure.fileStoreError != nil {
				shareFailure = &sessionFileShareFailure{status: http.StatusBadGateway, code: "file_store_refused",
					message: fmt.Sprintf("file-store: %v (the images were made and are at %s)", shareFailure.fileStoreError, generatedImagePaths(response.Images))}
			}
			shareFailure.write(w)
			return
		}
		response.SessionFiles = append(response.SessionFiles, record)
	}
	writeJSON(w, response)
}

// shareGeneratedImageIntoSession shares one image file the harness saved.
func (s *Server) shareGeneratedImageIntoSession(session *store.Session, image msg.GeneratedImage, maximumFileBytes int64, sharedBy msg.SessionFileSharer) (store.SessionFile, *sessionFileShareFailure) {
	if image.SizeBytes > maximumFileBytes {
		return store.SessionFile{}, &sessionFileShareFailure{status: http.StatusRequestEntityTooLarge, code: "file_too_large",
			message: fmt.Sprintf("%s is %d bytes; a session file may be at most %d (file-store's maximum_file_bytes)", image.Path, image.SizeBytes, maximumFileBytes)}
	}
	file, err := os.Open(image.Path)
	if err != nil {
		return store.SessionFile{}, &sessionFileShareFailure{status: http.StatusBadGateway, code: "generated_image_unreadable", message: err.Error()}
	}
	defer file.Close()
	filename := filepath.Base(image.Path)
	if problem := plainFilenameProblem(filename); problem != "" {
		return store.SessionFile{}, &sessionFileShareFailure{status: http.StatusBadGateway, code: "generated_image_unnamable", message: problem}
	}
	return s.shareFileIntoSession(session, file, maximumFileBytes, image.MediaType, filename, sharedBy)
}

func generatedImagePaths(images []msg.GeneratedImage) string {
	paths := make([]string, len(images))
	for index, image := range images {
		paths[index] = image.Path
	}
	return strings.Join(paths, ", ")
}
