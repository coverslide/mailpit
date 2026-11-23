# Tests and coverage

## Go coverage

Run the full Go suite and write a coverage profile:

```sh
go test -p 1 -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
go tool cover -html=coverage.out -o coverage.html
```

The CI test workflow runs the Go suite with coverage and prints the per-package/function summary in the job log. The profile can also be uploaded as a workflow artifact or sent to a coverage service if a repository chooses to configure one. No third-party coverage account or token is required to generate the report.

## Frontend end-to-end tests

Install dependencies with `npm ci`, then run:

```sh
npm run test:e2e
```

Playwright builds the frontend, starts Mailpit with an isolated temporary database, and checks desktop and mobile mailbox layouts in Chromium. Install its browser once with `npx playwright install chromium`, or set `PLAYWRIGHT_CHROMIUM_EXECUTABLE` to use a system Chromium binary.
