package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/kayushkin/aiauth"
	ms "github.com/kayushkin/model-store"
	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "ms",
		Short: "Model store — centralized model registry",
	}

	root.AddCommand(newServeCommand())

	// providers
	providersCommand := &cobra.Command{
		Use:   "providers",
		Short: "List configured providers",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := ms.Open("")
			if err != nil {
				return err
			}
			defer store.Close()

			providers, err := store.Providers()
			if err != nil {
				return err
			}

			fmt.Printf("%-14s %-20s\n", "ID", "Name")
			fmt.Println(strings.Repeat("─", 36))
			for _, p := range providers {
				fmt.Printf("%-14s %-20s\n", p.ID, p.Name)
			}
			return nil
		},
	}
	providersCommand.AddCommand(&cobra.Command{
		Use:   "add <id> <name>",
		Short: "Register one provider, leaving every other row as it is",
		Long: `Registers one provider without re-running seed, which rewrites every seeded
model row. Registering an existing id replaces its name.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := ms.Open("")
			if err != nil {
				return err
			}
			defer store.Close()

			if err := store.AddProvider(ms.Provider{ID: args[0], Name: args[1]}); err != nil {
				return err
			}
			fmt.Printf("Registered provider %s (%s).\n", args[0], args[1])
			return nil
		},
	})
	root.AddCommand(providersCommand)

	// models
	root.AddCommand(&cobra.Command{
		Use:   "models [provider]",
		Short: "List available models",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := ms.Open("")
			if err != nil {
				return err
			}
			defer store.Close()

			providers, _ := store.Providers()
			for _, p := range providers {
				if len(args) > 0 && args[0] != p.ID {
					continue
				}
				models, _ := store.Models(p.ID)
				if len(models) == 0 {
					continue
				}
				fmt.Printf("\n%s:\n", p.Name)
				for _, m := range models {
					aliases := ""
					if len(m.Aliases) > 0 {
						aliases = " (" + strings.Join(m.Aliases, ", ") + ")"
					}
					status := ""
					if !m.Enabled {
						status = " [disabled]"
					}
					fmt.Printf("  %-35s %-14s $%.2f/$%.2f per MTok%s%s\n", m.ID, m.ShortName, m.InputCost, m.OutputCost, aliases, status)
				}
			}
			return nil
		},
	})

	// resolve
	root.AddCommand(&cobra.Command{
		Use:   "resolve <model>",
		Short: "Resolve a model and show its details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := ms.Open("")
			if err != nil {
				return err
			}
			defer store.Close()

			model, err := store.ResolveModel(args[0])
			if err != nil {
				return err
			}

			fmt.Printf("Model:    %s (%s)\n", model.ID, model.Name)
			fmt.Printf("Short:    %s\n", model.ShortName)
			fmt.Printf("Provider: %s\n", model.Provider)
			fmt.Printf("Context:  %d tokens\n", model.MaxTokens)
			fmt.Printf("Cost:     $%.2f in / $%.2f out per MTok\n", model.InputCost, model.OutputCost)
			fmt.Printf("Enabled:  %v\n", model.Enabled)
			fmt.Printf("Priority: %d\n", model.Priority)
			return nil
		},
	})

	// seed
	root.AddCommand(&cobra.Command{
		Use:   "seed",
		Short: "Seed default providers and models",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := ms.Open("")
			if err != nil {
				return err
			}
			defer store.Close()

			if err := store.Seed(); err != nil {
				return err
			}
			fmt.Println("Seeded providers and models.")
			return nil
		},
	})

	// sync
	syncCmd := &cobra.Command{
		Use:   "sync [provider...]",
		Short: "Fetch models from provider APIs (anthropic, openai, google)",
		Long: `Fetches the live model list from each provider's API and upserts into the store.
Existing models are updated (name, context window) but user-set fields (enabled, priority, cost, aliases, short name) are preserved.
New models are added with default priority 100 and estimated costs.

Requires API keys via environment variables (ANTHROPIC_API_KEY, OPENAI_API_KEY, GOOGLE_API_KEY) or aiauth profiles.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := ms.Open("")
			if err != nil {
				return err
			}
			defer store.Close()

			providers := args
			if len(providers) == 0 {
				providers = []string{"anthropic", "openai", "google"}
			}

			envVars := map[string]string{
				"anthropic": "ANTHROPIC_API_KEY",
				"openai":    "OPENAI_API_KEY",
				"google":    "GOOGLE_API_KEY",
			}

			authStore := aiauth.DefaultStore()

			for _, p := range providers {
				envName, ok := envVars[p]
				if !ok {
					fmt.Printf("%-10s skipped (no sync support)\n", p)
					continue
				}
				// Try env var first, then aiauth
				key := os.Getenv(envName)
				if key == "" {
					resolved, err := authStore.ResolveKey(p)
					if err == nil && resolved != "" {
						key = resolved
					}
				}
				if key == "" {
					fmt.Printf("%-10s skipped (no credentials: set %s or add aiauth profile)\n", p, envName)
					continue
				}
				result, err := store.SyncProvider(p, key)
				if err != nil {
					fmt.Printf("%-10s error: %v\n", p, err)
					continue
				}
				fmt.Printf("%-10s +%d added, %d updated\n", p, result.Added, result.Updated)
			}
			return nil
		},
	}
	root.AddCommand(syncCmd)

	// enable/disable
	root.AddCommand(&cobra.Command{
		Use:   "enable <model>",
		Short: "Enable a model",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := ms.Open("")
			if err != nil {
				return err
			}
			defer store.Close()
			if err := store.SetEnabled(args[0], true); err != nil {
				return err
			}
			fmt.Printf("Enabled %s\n", args[0])
			return nil
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "disable <model>",
		Short: "Disable a model",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := ms.Open("")
			if err != nil {
				return err
			}
			defer store.Close()
			if err := store.SetEnabled(args[0], false); err != nil {
				return err
			}
			fmt.Printf("Disabled %s\n", args[0])
			return nil
		},
	})

	// cost
	root.AddCommand(&cobra.Command{
		Use:   "cost <model> <input> <output>",
		Short: "Set a model's input/output cost per million tokens",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			in, err := strconv.ParseFloat(args[1], 64)
			if err != nil {
				return fmt.Errorf("input cost %q is not a number: %w", args[1], err)
			}
			out, err := strconv.ParseFloat(args[2], 64)
			if err != nil {
				return fmt.Errorf("output cost %q is not a number: %w", args[2], err)
			}
			store, err := ms.Open("")
			if err != nil {
				return err
			}
			defer store.Close()
			if err := store.SetCost(args[0], in, out); err != nil {
				return err
			}
			fmt.Printf("Set %s cost to $%.2f in / $%.2f out per MTok\n", args[0], in, out)
			return nil
		},
	})

	// add
	var addName, addShortName string
	var addMaxTokens, addPriority int
	var addInputCost, addOutputCost float64
	addCmd := &cobra.Command{
		Use:   "add <provider> <model>",
		Short: "Add a model that sync cannot see (e.g. one only a subscription exposes)",
		Long: "Add a model by its exact provider id. Refuses an id or alias that already\n" +
			"exists, so it never overwrites a row's user-set fields. Costs default to 0:\n" +
			"leave them unset when no published price exists.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			providerID, modelID := args[0], args[1]
			store, err := ms.Open("")
			if err != nil {
				return err
			}
			defer store.Close()

			providers, err := store.Providers()
			if err != nil {
				return err
			}
			providerExists := false
			for _, p := range providers {
				if p.ID == providerID {
					providerExists = true
					break
				}
			}
			if !providerExists {
				return fmt.Errorf("provider %q not found; see `ms providers`", providerID)
			}
			if existing, err := store.ResolveModel(modelID); err == nil {
				return fmt.Errorf("%q already resolves to model %q", modelID, existing.ID)
			}

			name := addName
			if name == "" {
				name = modelID
			}
			if err := store.AddModel(ms.Model{
				ID: modelID, Provider: providerID, Name: name, ShortName: addShortName,
				MaxTokens: addMaxTokens, InputCost: addInputCost, OutputCost: addOutputCost,
				Enabled: true, Priority: addPriority,
			}); err != nil {
				return err
			}
			fmt.Printf("Added %s (%s)\n", modelID, providerID)
			return nil
		},
	}
	addCmd.Flags().StringVar(&addName, "name", "", "display name (default: the model id)")
	addCmd.Flags().StringVar(&addShortName, "short-name", "", "short display nickname")
	addCmd.Flags().IntVar(&addMaxTokens, "max-tokens", 0, "context window in tokens")
	addCmd.Flags().IntVar(&addPriority, "priority", 100, "failover priority (lower = preferred)")
	addCmd.Flags().Float64Var(&addInputCost, "input-cost", 0, "input cost per million tokens")
	addCmd.Flags().Float64Var(&addOutputCost, "output-cost", 0, "output cost per million tokens")
	root.AddCommand(addCmd)

	// priority
	root.AddCommand(&cobra.Command{
		Use:   "priority <model> <n>",
		Short: "Set a model's failover priority (lower = preferred)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("priority %q is not an integer: %w", args[1], err)
			}
			store, err := ms.Open("")
			if err != nil {
				return err
			}
			defer store.Close()
			if err := store.SetPriority(args[0], p); err != nil {
				return err
			}
			fmt.Printf("Set %s priority to %d\n", args[0], p)
			return nil
		},
	})

	// shortname
	root.AddCommand(&cobra.Command{
		Use:   "shortname <model> <name>",
		Short: "Set a model's short display nickname (e.g. opus-4.6)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := ms.Open("")
			if err != nil {
				return err
			}
			defer store.Close()
			if err := store.SetShortName(args[0], args[1]); err != nil {
				return err
			}
			fmt.Printf("Set %s short name to %s\n", args[0], args[1])
			return nil
		},
	})

	// alias (add / rm)
	aliasCmd := &cobra.Command{
		Use:   "alias",
		Short: "Manage model aliases",
	}
	aliasCmd.AddCommand(&cobra.Command{
		Use:   "add <model> <alias>",
		Short: "Point an alias at a model",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := ms.Open("")
			if err != nil {
				return err
			}
			defer store.Close()
			if err := store.AddAlias(args[0], args[1]); err != nil {
				return err
			}
			fmt.Printf("Aliased %s -> %s\n", args[1], args[0])
			return nil
		},
	})
	aliasCmd.AddCommand(&cobra.Command{
		Use:   "rm <alias>",
		Short: "Remove an alias",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := ms.Open("")
			if err != nil {
				return err
			}
			defer store.Close()
			if err := store.RemoveAlias(args[0]); err != nil {
				return err
			}
			fmt.Printf("Removed alias %s\n", args[0])
			return nil
		},
	})
	root.AddCommand(aliasCmd)

	// role: purpose-named pointers (best / default / balanced / efficient) into the registry
	roleCmd := &cobra.Command{
		Use:   "role",
		Short: "Manage model roles (best / default / balanced / efficient)",
	}
	roleCmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List role assignments",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := ms.Open("")
			if err != nil {
				return err
			}
			defer store.Close()
			roles, err := store.Roles()
			if err != nil {
				return err
			}
			for _, r := range ms.CanonicalRoles {
				if id, ok := roles[r]; ok {
					fmt.Printf("%-30s -> %s\n", r, id)
				} else {
					fmt.Printf("%-30s -> (unassigned)\n", r)
				}
			}
			return nil
		},
	})
	roleCmd.AddCommand(&cobra.Command{
		Use:   "set <role> <model>",
		Short: "Point a role at a model",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := ms.Open("")
			if err != nil {
				return err
			}
			defer store.Close()
			if err := store.SetRole(args[0], args[1]); err != nil {
				return err
			}
			fmt.Printf("Role %s -> %s\n", args[0], args[1])
			return nil
		},
	})
	roleCmd.AddCommand(&cobra.Command{
		Use:   "rm <role>",
		Short: "Clear a role assignment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := ms.Open("")
			if err != nil {
				return err
			}
			defer store.Close()
			if err := store.RemoveRole(args[0]); err != nil {
				return err
			}
			fmt.Printf("Cleared role %s\n", args[0])
			return nil
		},
	})
	root.AddCommand(roleCmd)

	// delete
	root.AddCommand(&cobra.Command{
		Use:   "delete <model>",
		Short: "Delete a model and its aliases",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := ms.Open("")
			if err != nil {
				return err
			}
			defer store.Close()
			if err := store.DeleteModel(args[0]); err != nil {
				return err
			}
			fmt.Printf("Deleted %s\n", args[0])
			return nil
		},
	})

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
