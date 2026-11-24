// Command eval-store serves the judged-run store (eval-store serve), imports
// the Python bench it replaces (eval-store import-legacy <dir>), and is a
// command line for the HTTP API (every other verb).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	evalstore "github.com/kayushkin/eval-store"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

const usage = `eval-store — judged runs of several models on a fixed set of real model calls

  eval-store serve                         run the HTTP server (EVAL_STORE_ADDR)
  eval-store import-legacy <dir>           import the Python bench in <dir> into the database directly

The rest talk to the server at EVAL_STORE_URL. A <json> argument is a file, or - for standard input.

  health | settings | vocabulary
  suites | suite <id> | suite-create <json> | suite-patch <id> <json> | suite-delete <id>
  cases <suite> | case <id> | case-create <suite> <json> | case-patch <id> <json> | import-operations <suite> <json>
  runs [suite] | run <id> | run-create <json> | run-patch <id> <json> | run-delete <id>
  start <run> | resume <run> [--retry-failed] | stop <run>
  answers <run> [--include-bodies] | verdicts <run> [--include-bodies] | checks <run> | recheck <run>
  report <run> [--markdown] [calls_per_week=N]
  cost-projection <run> [target_usd=100] [calls_per_week=N] [weekly.<feature>=N] [feature.<name>=N]
`

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("eval-store: ")
	settings, err := evalstore.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	verb, arguments := os.Args[1], os.Args[2:]
	switch verb {
	case "serve":
		serve(settings)
	case "import-legacy":
		importLegacy(settings, arguments)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		if err := runClientVerb(settings.String(evalstore.SettingServerURL), verb, arguments); err != nil {
			fmt.Fprintln(os.Stderr, "eval-store:", err)
			os.Exit(1)
		}
	}
}

func serve(settings *servicesettings.Registry) {
	if err := settings.CheckRequired(); err != nil {
		log.Fatalf("settings: %v", err)
	}
	store, err := evalstore.Open(settings.String(evalstore.SettingDataDirectory))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()
	if interrupted, err := store.MarkInterruptedRuns(); err != nil {
		log.Fatalf("mark interrupted runs: %v", err)
	} else if interrupted > 0 {
		log.Printf("%d runs were going when the service last stopped; they are interrupted and can be resumed", interrupted)
	}

	bridgeClient := &evalstore.BridgeClient{BaseURL: settings.String(evalstore.SettingLLMBridgeURL),
		ServiceToken: settings.String(evalstore.SettingLLMBridgeServiceToken),
		PollInterval: settings.Duration(evalstore.SettingOperationPollingInterval)}
	defaultLimits, err := evalstore.ParseBudgetLimits(settings.StringMap(evalstore.SettingDefaultBudgetLimits))
	if err != nil {
		log.Fatalf("settings: %s: %v", evalstore.SettingDefaultBudgetLimits, err)
	}
	runner := evalstore.NewRunner(store, bridgeClient, &evalstore.ModelStoreClient{BaseURL: settings.String(evalstore.SettingModelStoreURL)},
		&evalstore.UsageStoreClient{BaseURL: settings.String(evalstore.SettingUsageStoreURL)},
		evalstore.RunDefaults{Concurrency: settings.Integer(evalstore.SettingDefaultConcurrency), BudgetLimits: defaultLimits,
			BudgetCheckEveryCalls: settings.Integer(evalstore.SettingBudgetCheckEveryCalls), AnswerTimeout: settings.Duration(evalstore.SettingAnswerTimeout),
			JudgeTimeout: settings.Duration(evalstore.SettingJudgeTimeout), OrganizationID: settings.String(evalstore.SettingOrganizationID)})

	var bridge evalstore.Bridge
	if bridgeClient.BaseURL != "" {
		bridge = bridgeClient
	}
	mux := http.NewServeMux()
	evalstore.RegisterHandlers(mux, store, runner, bridge)
	evalstore.RegisterSettingsHandler(mux, settings)
	address := settings.String(evalstore.SettingListenAddress)
	server := &http.Server{Addr: address, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("listening on %s (data=%s)", address, store.DataDir())
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down; going runs end interrupted and their calls finish on the bridge")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(ctx)
	runner.Close()
}

func importLegacy(settings *servicesettings.Registry, arguments []string) {
	if len(arguments) != 1 {
		log.Fatalf("usage: eval-store import-legacy <dir>")
	}
	store, err := evalstore.Open(settings.String(evalstore.SettingDataDirectory))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()
	result, err := store.ImportLegacyBench(context.Background(), arguments[0],
		&evalstore.ModelStoreClient{BaseURL: settings.String(evalstore.SettingModelStoreURL)})
	if err != nil {
		log.Fatalf("import %s: %v", arguments[0], err)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	encoder.Encode(result)
}

func runClientVerb(serverURL, verb string, arguments []string) error {
	if serverURL == "" {
		return fmt.Errorf("EVAL_STORE_URL is not set; it is http://localhost:<port> with the port of EVAL_STORE_ADDR in eval-store.service")
	}
	flags, positional, query := splitArguments(arguments)
	need := func(n int) error {
		if len(positional) < n {
			return fmt.Errorf("%s needs %d argument(s); see eval-store help", verb, n)
		}
		return nil
	}
	escape := url.PathEscape
	var method, path string
	var body []byte
	var err error
	switch verb {
	case "health", "settings", "vocabulary":
		method, path = http.MethodGet, "/"+verb
	case "suites":
		method, path = http.MethodGet, "/suites"
	case "suite", "suite-delete":
		if err = need(1); err == nil {
			method, path = map[string]string{"suite": http.MethodGet, "suite-delete": http.MethodDelete}[verb], "/suites/"+escape(positional[0])
		}
	case "suite-create":
		if err = need(1); err == nil {
			method, path = http.MethodPost, "/suites"
			body, err = readBody(positional[0])
		}
	case "suite-patch":
		if err = need(2); err == nil {
			method, path = http.MethodPatch, "/suites/"+escape(positional[0])
			body, err = readBody(positional[1])
		}
	case "cases":
		if err = need(1); err == nil {
			method, path = http.MethodGet, "/suites/"+escape(positional[0])+"/cases"
		}
	case "case":
		if err = need(1); err == nil {
			method, path = http.MethodGet, "/cases/"+escape(positional[0])
		}
	case "case-patch":
		if err = need(2); err == nil {
			method, path = http.MethodPatch, "/cases/"+escape(positional[0])
			body, err = readBody(positional[1])
		}
	case "case-create", "import-operations":
		if err = need(2); err == nil {
			method, path = http.MethodPost, "/suites/"+escape(positional[0])+"/cases"
			if verb == "import-operations" {
				path += "/import-operations"
			}
			body, err = readBody(positional[1])
		}
	case "runs":
		method, path = http.MethodGet, "/runs"
		if len(positional) > 0 {
			path += "?suite_id=" + url.QueryEscape(positional[0])
		}
	case "run", "run-delete":
		if err = need(1); err == nil {
			method, path = map[string]string{"run": http.MethodGet, "run-delete": http.MethodDelete}[verb], "/runs/"+escape(positional[0])
		}
	case "run-create":
		if err = need(1); err == nil {
			method, path = http.MethodPost, "/runs"
			body, err = readBody(positional[0])
		}
	case "run-patch":
		if err = need(2); err == nil {
			method, path = http.MethodPatch, "/runs/"+escape(positional[0])
			body, err = readBody(positional[1])
		}
	case "start", "stop":
		if err = need(1); err == nil {
			method, path = http.MethodPost, "/runs/"+escape(positional[0])+"/"+verb
		}
	case "resume":
		if err = need(1); err == nil {
			method, path = http.MethodPost, "/runs/"+escape(positional[0])+"/resume"
			body, _ = json.Marshal(map[string]bool{"retry_failed": flags["retry-failed"]})
		}
	case "answers", "verdicts", "checks":
		if err = need(1); err == nil {
			method, path = http.MethodGet, "/runs/"+escape(positional[0])+"/"+verb
			if flags["include-bodies"] {
				path += "?include_bodies=true"
			}
		}
	case "recheck":
		if err = need(1); err == nil {
			method, path = http.MethodPost, "/runs/"+escape(positional[0])+"/checks"
		}
	case "report":
		if err = need(1); err == nil {
			if flags["markdown"] {
				query.Set("format", "markdown")
			}
			method, path = http.MethodGet, "/runs/"+escape(positional[0])+"/report?"+query.Encode()
		}
	case "cost-projection":
		if err = need(1); err == nil {
			method, path = http.MethodGet, "/runs/"+escape(positional[0])+"/cost-projection?"+query.Encode()
		}
	default:
		return fmt.Errorf("unknown verb %q; see eval-store help", verb)
	}
	if err != nil {
		return err
	}
	return call(serverURL, method, path, body)
}

// splitArguments separates --flags, key=value query parameters and the rest.
func splitArguments(arguments []string) (map[string]bool, []string, url.Values) {
	flags, query := map[string]bool{}, url.Values{}
	var positional []string
	for _, argument := range arguments {
		switch {
		case strings.HasPrefix(argument, "--"):
			flags[strings.TrimPrefix(argument, "--")] = true
		case strings.Contains(argument, "="):
			key, value, _ := strings.Cut(argument, "=")
			query.Set(key, value)
		default:
			positional = append(positional, argument)
		}
	}
	return flags, positional, query
}

func readBody(source string) ([]byte, error) {
	if source == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(source)
}

func call(serverURL, method, path string, body []byte) error {
	request, err := http.NewRequest(method, strings.TrimRight(serverURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode >= 300 {
		return fmt.Errorf("%s %s answered %s: %s", method, path, response.Status, strings.TrimSpace(string(data)))
	}
	os.Stdout.Write(data)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		fmt.Println()
	}
	return nil
}
