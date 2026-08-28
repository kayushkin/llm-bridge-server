package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/kayushkin/aiauth"
	"github.com/kayushkin/aiauth/providers"
	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "aiauth",
		Short: "LLM provider auth management",
	}

	root.AddCommand(loginCmd(), statusCmd(), keyCmd(), refreshCmd())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func loginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login [provider]",
		Short: "Authenticate with a provider via OAuth",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			provider := args[0]
			if provider != "anthropic" {
				return fmt.Errorf("unsupported provider: %s", provider)
			}

			store := aiauth.DefaultStore()
			ap := providers.NewAnthropic()

			cred, err := ap.Login(aiauth.LoginCallbacks{
				OnAuthURL: func(url string) error {
					fmt.Println("Open this URL in your browser:")
					fmt.Println(url)
					openBrowser(url)
					return nil
				},
				OnPrompt: func(message string) (string, error) {
					fmt.Print(message + " ")
					var code string
					_, err := fmt.Scanln(&code)
					return code, err
				},
			})
			if err != nil {
				return err
			}

			// Save as oauth profile (canonical)
			if err := store.SetProfile("anthropic:oauth", cred); err != nil {
				return fmt.Errorf("failed to save oauth profile: %w", err)
			}

			// Also update anthropic:manual (token type) for OpenClaw compatibility.
			// OpenClaw's lastGood often points to anthropic:manual, so keep it fresh.
			manualCred := &aiauth.Credential{
				Type:     "token",
				Provider: "anthropic",
				Token:    cred.Access,
				Expires:  cred.Expires,
				Email:    cred.Email,
			}
			if err := store.SetProfile("anthropic:manual", manualCred); err != nil {
				return fmt.Errorf("failed to save manual profile: %w", err)
			}

			fmt.Println("✓ Logged in successfully")

			// Sync to model-store (updates inber's credential DB)
			syncModelStore()

			return nil
		},
	}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show all configured providers and credential status",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			store := aiauth.DefaultStore()
			profiles := store.Profiles()
			if len(profiles) == 0 {
				fmt.Fprintln(out, "No credentials configured.")
				return nil
			}
			for name, c := range profiles {
				status := "valid"
				masked := ""
				switch c.Type {
				case "oauth":
					masked = aiauth.MaskKey(c.Access)
					if c.Expires > 0 && c.Expires < time.Now().UnixMilli() {
						status = "expired"
					}
				case "token":
					masked = aiauth.MaskKey(c.Token)
					if c.Expires > 0 && c.Expires < time.Now().UnixMilli() {
						status = "expired"
					}
				case "api_key":
					masked = aiauth.MaskKey(c.Key)
				}
				fmt.Fprintf(out, "%-25s  type=%-7s  provider=%-10s  key=%s  status=%s\n",
					name, c.Type, c.Provider, masked, status)
			}
			return nil
		},
	}
}

func keyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "key [provider]",
		Short: "Print resolved API key to stdout",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			aiauth.RegisterProvider(&providers.Anthropic{})
			store := aiauth.DefaultStore()
			key, err := store.ResolveKey(args[0])
			if err != nil {
				return err
			}
			fmt.Print(key)
			return nil
		},
	}
}

func refreshCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh [provider]",
		Short: "Manually refresh OAuth token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			provider := args[0]
			if provider != "anthropic" {
				return fmt.Errorf("unsupported provider: %s", provider)
			}

			store := aiauth.DefaultStore()
			creds := store.ProfilesForProvider(provider)

			for _, c := range creds {
				if c.Type != "oauth" {
					continue
				}
				ap := providers.NewAnthropic()
				refreshed, err := ap.RefreshToken(c)
				if err != nil {
					return fmt.Errorf("refresh failed: %w", err)
				}
				name := store.FindProfileName(c)
				if err := store.UpdateProfile(name, refreshed); err != nil {
					return fmt.Errorf("failed to save: %w", err)
				}

				// Also sync to anthropic:manual for OpenClaw compatibility
				manualCred := &aiauth.Credential{
					Type:     "token",
					Provider: "anthropic",
					Token:    refreshed.Access,
					Expires:  refreshed.Expires,
					Email:    refreshed.Email,
				}
				if err := store.UpdateProfile("anthropic:manual", manualCred); err != nil {
					return fmt.Errorf("failed to sync manual profile: %w", err)
				}

				fmt.Println("✓ Token refreshed successfully")
				syncModelStore()
				return nil
			}
			return fmt.Errorf("no OAuth credentials found for %s", provider)
		},
	}
}

// syncModelStore notifies model-store to pull updated credentials from auth-profiles.json.
func syncModelStore() {
	addr := os.Getenv("MODEL_STORE_URL")
	if addr == "" {
		addr = "http://localhost:8150"
	}
	resp, err := http.Post(addr+"/api/credentials/sync", "application/json", nil)
	if err != nil {
		fmt.Printf("⚠ model-store sync failed (is it running?): %v\n", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode == 200 {
		fmt.Println("✓ model-store synced")
	} else {
		fmt.Printf("⚠ model-store sync returned %d\n", resp.StatusCode)
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	default:
		return
	}
	_ = cmd.Start()
}
