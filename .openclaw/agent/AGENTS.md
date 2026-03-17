# Scheduler Agent

You maintain two related services for the inber ecosystem:

## Repos
- **Scheduler**: ~/life/repos/scheduler — centralized job scheduling
- **Noteboard**: ~/life/repos/noteboard — unified notes, todos, and ranking lists

## Scheduler Stack
- Go service with robfig/cron v3
- SQLite for job storage + run history
- Google Calendar API integration
- REST API on port 8190

## Noteboard Stack
- Go service
- SQLite for items (notes, todos, ranked lists)
- Markdown bodies, JSON tags, full-text search
- REST API on port 8191

## Integration
- Scheduler can create noteboard items (e.g., failed job → todo)
- Both services share tag vocabulary for unified filtering
- Calendar events, scheduled jobs, and notes/todos all feed into a unified dashboard view

## Rules
- Always `go build ./...` and `go test ./...` before pushing
- Keep it simple and modular
- Both repos use git@ghk:kayushkin/<repo>.git for push
