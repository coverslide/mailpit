import { defineConfig } from "@playwright/test";

const baseURL = "http://127.0.0.1:18025";

export default defineConfig({
	testDir: "./e2e",
	fullyParallel: true,
	reporter: "list",
	use: {
		baseURL,
		browserName: "chromium",
		launchOptions: {
			executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE,
			args: ["--no-sandbox"],
		},
		viewport: { width: 1440, height: 900 },
		trace: "retain-on-failure",
	},
	webServer: {
		command:
			"npm run build && go run . --listen 127.0.0.1:18025 --smtp 127.0.0.1:0 --pop3 127.0.0.1:0 --imap 127.0.0.1:0 --database /tmp/mailpit-playwright.db --disable-version-check",
		url: baseURL,
		reuseExistingServer: !process.env.CI,
		timeout: 120_000,
		env: {
			GOCACHE: "/tmp/mailpit-playwright-gocache",
			GOPATH: "/tmp/mailpit-playwright-gopath",
		},
	},
});
