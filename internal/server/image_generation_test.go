package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	harnessstore "github.com/kayushkin/harness-store"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge/msg"
)

// testServerForImages is a server with file-store and a codex instance named
// by image_generation.instance_id, whose harness binary is a script on PATH.
// The script records its arguments and stdin, writes one PNG, and answers
// with harnessAnswer — or, when harnessAnswer is empty, a response listing
// that PNG.
func testServerForImages(t *testing.T, harnessAnswer string, harnessExitCode int) (srv *Server, imageDirectory string, harnessRecordDirectory string) {
	t.Helper()
	srv, _, _, _ = testServerWithFileStore(t)
	dir := t.TempDir()
	harnesses, err := harnessstore.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { harnesses.Close() })
	if err := harnesses.CreateMachine(&msg.Machine{ID: "m_images", Name: "images", Hostname: "localhost", Transport: msg.TransportLocal}); err != nil {
		t.Fatal(err)
	}
	if err := harnesses.CreateInstance(&msg.Instance{ID: "inst_images", HarnessType: msg.HarnessCodex, Name: "images", MachineID: "m_images", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	srv.harnessStore = harnesses
	srv.wireServiceSettings()
	if err := srv.settings.Set(config.SettingImageGenerationInstanceID, "inst_images"); err != nil {
		t.Fatal(err)
	}

	imageDirectory = filepath.Join(dir, "generated")
	harnessRecordDirectory = filepath.Join(dir, "record")
	for _, directory := range []string{imageDirectory, harnessRecordDirectory} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	imagePath := filepath.Join(imageDirectory, "exec-1.png")
	if harnessAnswer == "" {
		answer, _ := json.Marshal(msg.ImageGenerationResponse{
			Images: []msg.GeneratedImage{{Path: imagePath, MediaType: "image/png", SizeBytes: int64(len(onePixelPNGForImageTests))}},
			Text:   "Made a crab.",
		})
		harnessAnswer = string(answer)
	}
	binaryDirectory := t.TempDir()
	script := "#!/bin/sh\n" +
		"echo \"$@\" > " + filepath.Join(harnessRecordDirectory, "arguments") + "\n" +
		"cat > " + filepath.Join(harnessRecordDirectory, "stdin") + "\n" +
		"printf '%s' '" + string(onePixelPNGForImageTests) + "' > " + imagePath + "\n" +
		"printf '%s' '" + harnessAnswer + "'\n" +
		"exit " + string(rune('0'+harnessExitCode)) + "\n"
	if err := os.WriteFile(filepath.Join(binaryDirectory, msg.HarnessBinaryName(msg.HarnessCodex)), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binaryDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	return srv, imageDirectory, harnessRecordDirectory
}

// Not a decodable PNG, but its first bytes are PNG's, which is all the
// server and file-store look at.
var onePixelPNGForImageTests = []byte("\x89PNG-test-bytes")

func TestAnAgentMakesAnImageAndItIsSharedIntoItsOwnSession(t *testing.T) {
	srv, _, harnessRecordDirectory := testServerForImages(t, "", 0)
	session := newSessionForSignals(t, srv.store, "br_images", msg.SessionTypeInteractive)
	environment := map[string]string{}
	for _, variable := range srv.sessionPostingEnvironment(session) {
		name, value, _ := strings.Cut(variable, "=")
		environment[name] = value
	}
	if environment[sessionImagesURLEnvironmentVariable] != "http://127.0.0.1:8160/sessions/br_images/images" {
		t.Fatalf("child environment = %v", environment)
	}
	events := srv.harness.Subscribe("br_images")

	body := `{"prompt":"a red crab","reference_image_paths":["/tmp/reference.png"]}`
	request := httptest.NewRequest(http.MethodPost, "/sessions/br_images/images", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+environment[sessionPostingTokenEnvironmentVariable])
	recorder := httptest.NewRecorder()
	srv.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("make images: %d %s", recorder.Code, recorder.Body)
	}
	var response msg.ImageGenerationResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Images) != 1 || len(response.SessionFiles) != 1 || response.InstanceID != "inst_images" || response.Text != "Made a crab." {
		t.Fatalf("response = %+v", response)
	}
	shared := response.SessionFiles[0]
	if shared.SharedBy != msg.SessionFileSharedByAgent || shared.Filename != "exec-1.png" || shared.SessionID != "br_images" {
		t.Errorf("session file = %+v", shared)
	}
	if onDisk, err := os.ReadFile(shared.Path); err != nil || string(onDisk) != string(onePixelPNGForImageTests) {
		t.Errorf("agent copy = %q, %v", onDisk, err)
	}
	select {
	case event := <-events:
		if event.Type != msg.EventSessionFile || event.SessionFile == nil || event.SessionFile.FileID != shared.FileID {
			t.Errorf("event = %+v", event)
		}
	default:
		t.Error("no session_file event was written")
	}

	arguments, _ := os.ReadFile(filepath.Join(harnessRecordDirectory, "arguments"))
	if strings.TrimSpace(string(arguments)) != "-generate-image" {
		t.Errorf("harness arguments = %q", arguments)
	}
	var sent msg.ImageGenerationRequest
	stdin, _ := os.ReadFile(filepath.Join(harnessRecordDirectory, "stdin"))
	if err := json.Unmarshal(stdin, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Prompt != "a red crab" || len(sent.ReferenceImagePaths) != 1 || sent.Caller != "session br_images" {
		t.Errorf("harness was sent %+v", sent)
	}
}

func TestTheOperatorMakesAnImageOutsideAnySession(t *testing.T) {
	srv, imageDirectory, _ := testServerForImages(t, "", 0)
	response := doJSON(t, asInternalService(srv), http.MethodPost, "/images", msg.ImageGenerationRequest{Prompt: "a red crab", Caller: "test"})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST /images: %d", response.StatusCode)
	}
	var answer msg.ImageGenerationResponse
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		t.Fatal(err)
	}
	if len(answer.Images) != 1 || answer.Images[0].Path != filepath.Join(imageDirectory, "exec-1.png") || len(answer.SessionFiles) != 0 {
		t.Errorf("answer = %+v", answer)
	}
}

func TestTheHarnessesOwnErrorIsPassedOn(t *testing.T) {
	srv, _, _ := testServerForImages(t, `{"error":"the turn saved no image"}`, 1)
	recorder := httptest.NewRecorder()
	asInternalService(srv).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/images", strings.NewReader(`{"prompt":"a crab"}`)))
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "the turn saved no image") {
		t.Errorf("got %d %s", recorder.Code, recorder.Body)
	}
}

func TestImageRequestsAreCheckedBeforeAnyHarnessRuns(t *testing.T) {
	srv, _, harnessRecordDirectory := testServerForImages(t, "", 0)
	for body, wantCode := range map[string]string{
		`{"prompt":"  "}`: "prompt_required",
		`{"prompt":"a crab","reference_image_paths":["relative.png"]}`: "reference_image_path_not_absolute",
		`not json`: "invalid_body",
	} {
		recorder := httptest.NewRecorder()
		asInternalService(srv).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/images", strings.NewReader(body)))
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), wantCode) {
			t.Errorf("%s: got %d %s, want 400 %s", body, recorder.Code, recorder.Body, wantCode)
		}
	}
	if _, err := os.Stat(filepath.Join(harnessRecordDirectory, "arguments")); !os.IsNotExist(err) {
		t.Error("the harness ran for a refused request")
	}
}

func TestAnEmptyInstanceSettingSwitchesImagesOff(t *testing.T) {
	srv, _, _ := testServerForImages(t, "", 0)
	if err := srv.settings.Set(config.SettingImageGenerationInstanceID, ""); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	asInternalService(srv).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/images", strings.NewReader(`{"prompt":"a crab"}`)))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "image_generation_off") {
		t.Errorf("got %d %s", recorder.Code, recorder.Body)
	}
}

func TestTheImageInstanceSettingRefusesAnUnknownInstance(t *testing.T) {
	srv, _, _ := testServerForImages(t, "", 0)
	if err := srv.settings.Set(config.SettingImageGenerationInstanceID, "inst_nowhere"); err == nil {
		t.Error("an unknown instance was accepted")
	}
}
