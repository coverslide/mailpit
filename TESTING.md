# Tests and coverage

## Go coverage

Run the full Go suite and write a coverage profile:

```sh
go test -p 1 -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
go tool cover -html=coverage.out -o coverage.html
```

The CI test workflow runs the Go suite with coverage and prints the per-package/function summary in the job log. Gitea also saves a `go-coverage` artifact for 30 days containing `index.html` (clickable source coverage), `summary.txt`, `coverage.out`, the commit ID, and test outcome. Reports from failed test runs may be incomplete and are labeled accordingly. Instance retention limits can shorten the requested retention period. No third-party coverage account or token is required.

To view a saved report, open the repository's **Actions** tab, select a **Tests** run, and select the `go-coverage` artifact. If your instance supports HTML artifact previews, open `index.html` there; otherwise download and extract the artifact, then open `index.html` in a browser. The HTML includes the source needed to view covered and uncovered lines.

## Frontend end-to-end tests

Install dependencies with `npm ci`, then run:

```sh
npm run test:e2e
```

The CI workflows build the UI and Mailpit binary before Playwright starts Mailpit with an isolated temporary database. The E2E suite checks mailbox actions, message views, search, desktop and mobile layouts, and webroot routing. Install Chromium with `npx playwright install chromium`, or set `PLAYWRIGHT_CHROMIUM_EXECUTABLE` to use a system Chromium binary.

Gitea's **Tests (e2e)** workflow saves a `playwright-report` artifact for 14 days on both passing and failing test runs. It contains the HTML test report and available failure traces. Download and extract it, then run `npx playwright show-report <path-to-playwright-report>` to view the report, or `npx playwright show-trace <path-to-trace.zip>` to investigate a failure. This is a test-results report, not frontend JavaScript code coverage.

Both Gitea workflows pin a Gitea-compatible artifact uploader to a specific commit. The action is fetched from GitHub, but report files are uploaded to this Gitea instance's artifact storage; no GitHub artifact service or external coverage service is used.
