# rule-store

Rules that react to changes in other services, held as data and run against
llm-bridge-server's session feed. Every rule runs in shadow mode until it has
been compared with the binary it replaces. See `CONTRACT.md`.
