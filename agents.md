# OpenRouter Free LLM Gateway (agents.md)

This file contains instructions, conventions, and operational patterns for any AI agents or developers working on the `openrouter-gateway` project.

## 🛠️ Stack & Architecture Overview
- **Language:** Go (1.26.3)
- **Database:** SQLite via pure Go driver (`modernc.org/sqlite`) - **Do not use CGO or any C-bound drivers.**
- **Routing:** Standard library `net/http` (no complex router/frameworks).
- **Core Components:**
  - `cmd/gateway/main.go`: Entry point, lifecycle management, HTTP server orchestration, and graceful shutdown.
  - `internal/config`: Configurations via flags and environment variables.
  - `internal/store`: Database layer for API keys, usage statistics, logs, and ranking.
  - `internal/keys`: 
    - `KeyPool`: Thread-safe pool managing key allocation, status rotation (`Active`, `Cooldown`, `Exhausted`, `Invalid`), and minute/daily quotas.
    - `KeyChecker`: Background ticker worker that validates key active statuses/limits with OpenRouter `/api/v1/key` endpoint.
  - `internal/models`: Periodically fetches the model rank top from Shir-Man API, resolving aliases like `top1`/`top2`/`top3`.
  - `internal/proxy`: Thread-safe reverse proxy (`/v1/chat/completions`) implementing auto-retry on fallback keys (up to 5 times) and proxying requested payloads.
  - `internal/web`: Live HTML dashboard with key management, statistics visualizations, and actions protected via Basic Auth.

## 📜 Development Guidelines & Rules (Ponytail-Friendly)
1. **Zero Over-Engineering:** Keep standard library solutions first. Do not add routing, ORM, or state-management packages. Use raw SQL/prepared statements inside `store.go`.
2. **Concurrency Safety:** `KeyPool` and checkers operate concurrently. Always guard map/slice/counter reads and writes with read-write mutexes (`sync.RWMutex`).
3. **Robust Retries & Error States:** In `internal/proxy`, errors like `429` (too many requests), `401` (unauthorized), and `5xx` must not immediately propagate to the client. Mark the offending key, activate cooldown/invalid status, and retry immediately using another healthy key.
4. **No SQLite CGO:** Keep SQLite purely serverless and C-dependency free.
5. **Add High-Value Tests only:** Code changes that modify the rotation logic or check criteria should be covered in `sqlite_test.go`, `state_test.go`, or `server_test.go`.
6. **Language Constraint:** **ALWAYS respond in Russian.** All communication with the user must be in Russian only. Code comments can remain in English/Russian matching existing files, but explanations, summaries, and agent output must be Russian.

## ⚙️ Key Commands
- **Run local app:** `go run cmd/gateway/main.go`
- **Run all unit tests:** `go test ./...`
- **Lint/Format:** `go fmt ./...`
- **Build binary:** `go build -o build/gateway.exe cmd/gateway/main.go`
