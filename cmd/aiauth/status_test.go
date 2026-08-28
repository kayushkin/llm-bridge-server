package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/aiauth"
)

// `aiauth status` is the one command that prints a credential. It prints the masked
// form, and `MaskKey` is pinned by `TestMaskKey` in the root package — but that test
// holds the helper, not the three lines in this file that decide whether the helper is
// asked. Before these tests existed, replacing `aiauth.MaskKey(c.Access)` with
// `c.Access` printed a live OAuth token to the terminal and reddened nothing.
//
// Both halves below are load-bearing. Asserting only that the secret is absent is
// satisfied by a command that prints nothing at all, so each case also asserts the
// masked form is present. Scored by scripts/sabotage-mask-call-sites.py.

// secretsInStore seeds a throwaway HOME with the auth-profiles.json that
// aiauth.DefaultStore reads, and returns the file's path.
func seedStore(t *testing.T, profiles map[string]*aiauth.Credential) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".openclaw", "agents", "main", "agent", "auth-profiles.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body, err := json.Marshal(map[string]any{"version": 1, "profiles": profiles})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// runStatus executes the real `status` command against whatever HOME currently points
// at and returns everything it wrote.
func runStatus(t *testing.T) string {
	t.Helper()
	cmd := statusCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("status: %v", err)
	}
	return out.String()
}

func TestStatusNeverPrintsARawCredential(t *testing.T) {
	// Each secret is long enough that MaskKey abbreviates rather than starring it out,
	// and none is a substring of the surrounding report.
	cases := []struct {
		name    string
		profile string
		cred    *aiauth.Credential
		secret  string
	}{
		{
			name:    "oauth access token",
			profile: "anthropic:oauth",
			cred: &aiauth.Credential{
				Type: "oauth", Provider: "anthropic",
				Access:  "sk-ant-oat01-ZQ7yRAWACCESSVALUE-wxyz",
				Refresh: "sk-ant-ort01-refreshsecretvalue-abcd",
			},
			secret: "sk-ant-oat01-ZQ7yRAWACCESSVALUE-wxyz",
		},
		{
			name:    "bearer token",
			profile: "anthropic:manual",
			cred: &aiauth.Credential{
				Type: "token", Provider: "anthropic",
				Token: "sk-ant-tok01-KP3mRAWTOKENVALUE-stuv",
			},
			secret: "sk-ant-tok01-KP3mRAWTOKENVALUE-stuv",
		},
		{
			name:    "api key",
			profile: "openai:default",
			cred: &aiauth.Credential{
				Type: "api_key", Provider: "openai",
				Key: "sk-proj-J8nRAWAPIKEYVALUE-mnop",
			},
			secret: "sk-proj-J8nRAWAPIKEYVALUE-mnop",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seedStore(t, map[string]*aiauth.Credential{tc.profile: tc.cred})
			out := runStatus(t)

			if strings.Contains(out, tc.secret) {
				t.Errorf("the raw %s reached the terminal.\nsecret: %q\noutput:\n%s",
					tc.name, tc.secret, out)
			}
			// Absence alone is satisfied by printing nothing, so the masked form has
			// to be there too, or this test cannot tell masking from silence.
			want := aiauth.MaskKey(tc.secret)
			if !strings.Contains(out, want) {
				t.Errorf("masked %s missing from the report.\nwant substring: %q\noutput:\n%s",
					tc.name, want, out)
			}
			if !strings.Contains(out, tc.profile) {
				t.Errorf("profile name %q missing from the report:\n%s", tc.profile, out)
			}
		})
	}
}

// All three credential types in one report, because the command loops over a map and a
// single-profile fixture cannot show that one branch of the switch does not leak while
// another does.
func TestStatusMasksEveryCredentialTypeInOneReport(t *testing.T) {
	secrets := map[string]string{
		"access": "sk-ant-oat01-ZQ7yRAWACCESSVALUE-wxyz",
		"token":  "sk-ant-tok01-KP3mRAWTOKENVALUE-stuv",
		"key":    "sk-proj-J8nRAWAPIKEYVALUE-mnop",
	}
	seedStore(t, map[string]*aiauth.Credential{
		"anthropic:oauth":  {Type: "oauth", Provider: "anthropic", Access: secrets["access"]},
		"anthropic:manual": {Type: "token", Provider: "anthropic", Token: secrets["token"]},
		"openai:default":   {Type: "api_key", Provider: "openai", Key: secrets["key"]},
	})

	out := runStatus(t)
	for which, secret := range secrets {
		if strings.Contains(out, secret) {
			t.Errorf("raw %s credential reached the terminal:\n%s", which, out)
		}
		if !strings.Contains(out, aiauth.MaskKey(secret)) {
			t.Errorf("masked %s credential missing from the report:\n%s", which, out)
		}
	}
	if n := strings.Count(out, "\n"); n != 3 {
		t.Errorf("want one line per profile (3), got %d:\n%s", n, out)
	}
}

func TestStatusSaysSoWhenThereAreNoCredentials(t *testing.T) {
	seedStore(t, map[string]*aiauth.Credential{})
	if out := runStatus(t); !strings.Contains(out, "No credentials configured.") {
		t.Errorf("want the empty-store message, got %q", out)
	}
}
