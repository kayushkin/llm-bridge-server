// Package schedulersettings is how a job binary reads a setting the scheduler
// service stores for it: GET {scheduler}/settings, one key.
//
// A job binary is a short-lived process with no store of its own, so a
// setting an operator changes from a settings page has to live in the
// service. The binary reads it once when it starts; there is no copy in its
// environment or its flags that could win instead.
package schedulersettings

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// ReadModelRole returns the model-store role stored under key. It fails when
// the scheduler cannot be read, when it declares no such setting (a service
// older than this binary), when the setting is not a model role, or when no
// role is in force.
func ReadModelRole(ctx context.Context, schedulerURL, key string) (string, error) {
	if schedulerURL == "" {
		return "", fmt.Errorf("read model role %s: the scheduler URL is empty", key)
	}
	url := strings.TrimRight(schedulerURL, "/") + "/settings"
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("read model role %s: %w", key, err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("read model role %s from the scheduler: %w", key, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("read model role %s: read %s: %w", key, url, err)
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("read model role %s: GET %s: %s: %s", key, url, response.Status, strings.TrimSpace(string(body)))
	}
	var described msg.ServiceSettings
	if err := json.Unmarshal(body, &described); err != nil {
		return "", fmt.Errorf("read model role %s: decode GET %s: %w", key, url, err)
	}
	for _, setting := range described.Settings {
		if setting.Key != key {
			continue
		}
		if setting.ValueType != msg.ServiceSettingValueTypeModelRole {
			return "", fmt.Errorf("read model role %s: the scheduler declares it as %s, not %s", key, setting.ValueType, msg.ServiceSettingValueTypeModelRole)
		}
		if setting.Value == "" {
			return "", fmt.Errorf("read model role %s: the scheduler holds no role for it; set one with PUT %s/%s {\"value\":\"balanced\"}", key, url, key)
		}
		return setting.Value, nil
	}
	return "", fmt.Errorf("read model role %s: the scheduler at %s declares no such setting; it is older than this binary and needs deploying", key, schedulerURL)
}
