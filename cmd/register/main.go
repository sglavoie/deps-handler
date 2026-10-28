// Command register creates a GitHub App from a manifest, so registering
// it takes two clicks on GitHub instead of a form:
//
//	go run ./cmd/register                              # the private dev App
//	go run ./cmd/register -public -name deps-merger    # the production App
//
// It serves a page on localhost that posts the manifest to GitHub. Once
// the App is created, it saves the private key and writes
// ~/.config/deps-merger-cloud/<profile>.env with DMC_APP_ID and
// DMC_PRIVATE_KEY_FILE, then sends you to install the App. When you have
// installed it, it adds DMC_INSTALLATION_ID and exits. The profile is dev,
// or prod with -public, and an existing one is never overwritten.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var page = template.Must(template.New("").Parse(`<!doctype html>
<meta charset="utf-8">
<title>Register deps-merger dev App</title>
<form action="https://github.com/settings/apps/new" method="post">
<input type="hidden" name="manifest" value="{{.}}">
<p>This creates a deps-merger GitHub App. Choose its visibility with the command flags.</p>
<button type="submit">Create the App on GitHub</button>
</form>`))

func main() {
	name := flag.String("name", "deps-merger-dev-sglavoie", "App name (unique across GitHub)")
	public := flag.Bool("public", false, "let any account install it, as the production App must; the dev App stays private")
	profile := flag.String("profile", "", "name of the settings and key files written (default dev, or prod with -public)")
	webhook := flag.String("webhook", "", "deliver webhooks to this https URL from the start, with a secret GitHub makes; without it the webhook is off")
	port := flag.Int("port", 8787, "localhost port to serve on")
	flag.Parse()
	if *profile == "" {
		*profile = map[bool]string{false: "dev", true: "prod"}[*public]
	}
	if *public && *name == "deps-merger-dev-sglavoie" {
		fmt.Fprintln(os.Stderr, "register: name the production App with -name")
		os.Exit(2)
	}
	if *webhook != "" && !strings.HasPrefix(*webhook, "https://") {
		fmt.Fprintln(os.Stderr, "register: -webhook must be an https URL")
		os.Exit(2)
	}
	if err := run(*name, *port, *public, *profile, *webhook); err != nil {
		fmt.Fprintln(os.Stderr, "register:", err)
		os.Exit(1)
	}
}

func run(name string, port int, public bool, profile, webhook string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".config", "deps-merger-cloud")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	envFile := filepath.Join(dir, profile+".env")
	keyFile := filepath.Join(dir, profile+".pem")
	for _, f := range []string{envFile, keyFile} {
		if _, err := os.Stat(f); err == nil {
			return fmt.Errorf("%s exists: it holds another App's settings; pick another -profile", f)
		}
	}

	base := fmt.Sprintf("http://localhost:%d", port)
	manifest, err := json.Marshal(map[string]any{
		"name":         name,
		"url":          "https://github.com/sglavoie/deps-merger",
		"redirect_url": base + "/created",
		"setup_url":    base + "/installed",
		"public":       public,
		// GitHub requires a webhook URL even when deliveries are disabled.
		"hook_attributes": hook(webhook),
		// Events the App needs from the start: ones added later must be
		// accepted on each installation before GitHub delivers any.
		"default_events": []string{"check_suite", "pull_request", "push", "status"},
		"default_permissions": map[string]string{
			"metadata":      "read",
			"contents":      "write",
			"pull_requests": "write",
			"issues":        "write",
			"checks":        "read",
			// For check suites' workflow runs, which the engine reads to
			// tell reruns apart; private repositories refuse them without.
			"actions":   "read",
			"statuses":  "read",
			"workflows": "write",
		},
	})
	if err != nil {
		return err
	}

	done := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		page.Execute(w, string(manifest))
	})
	mux.HandleFunc("GET /created", func(w http.ResponseWriter, r *http.Request) {
		app, err := convert(r.Context(), r.URL.Query().Get("code"))
		if err == nil {
			err = save(envFile, keyFile, filepath.Join(home, ".local", "state", "deps-merger-cloud", profile+".db"), app)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			done <- err
			return
		}
		fmt.Printf("created App %s (ID %d); key saved to %s\n", app.Slug, app.ID, keyFile)
		// Install it next; GitHub comes back to setup_url with the
		// installation ID.
		http.Redirect(w, r, app.HTMLURL+"/installations/new", http.StatusFound)
	})
	mux.HandleFunc("GET /installed", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("installation_id")
		if id == "" {
			http.Error(w, "no installation_id", http.StatusBadRequest)
			return
		}
		f, err := os.OpenFile(envFile, os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			_, err = fmt.Fprintf(f, "DMC_INSTALLATION_ID=%s\n", id)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			done <- err
			return
		}
		fmt.Fprintln(w, "Installed. You can close this tab.")
		fmt.Printf("installed (installation %s); settings in %s\n", id, envFile)
		done <- nil
	})

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	fmt.Printf("open %s and follow the steps on GitHub\n", base)
	err = <-done
	// Let the last response reach the browser.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	return err
}

type createdApp struct {
	ID      int64  `json:"id"`
	Slug    string `json:"slug"`
	HTMLURL string `json:"html_url"`
	PEM     string `json:"pem"`
	// WebhookSecret is the secret GitHub made for the webhook.
	WebhookSecret string `json:"webhook_secret"`
}

// hook is the manifest's webhook: on, delivering to url, or off with a
// placeholder URL, which GitHub requires all the same.
func hook(url string) map[string]any {
	if url == "" {
		return map[string]any{"url": "https://example.com/deps-merger", "active": false}
	}
	return map[string]any{"url": url, "active": true}
}

// convert trades the one-time code GitHub redirects with for the new App's
// credentials. The code alone authenticates the call, for an hour.
func convert(ctx context.Context, code string) (*createdApp, error) {
	if code == "" {
		return nil, errors.New("GitHub sent no code")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.github.com/app-manifests/"+code+"/conversions", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("manifest conversion: %s", resp.Status)
	}
	var app createdApp
	if err := json.NewDecoder(resp.Body).Decode(&app); err != nil {
		return nil, err
	}
	if app.ID == 0 || app.PEM == "" {
		return nil, errors.New("manifest conversion returned no App ID or key")
	}
	return &app, nil
}

// save writes the App's key and its settings: its ID, key file, webhook
// secret when GitHub made one, and a database of its own, so that two
// Apps' records never mix.
func save(envFile, keyFile, db string, app *createdApp) error {
	if err := os.WriteFile(keyFile, []byte(app.PEM), 0o600); err != nil {
		return err
	}
	env := fmt.Sprintf("DMC_APP_ID=%d\nDMC_PRIVATE_KEY_FILE=%s\nDMC_DB=%s\n", app.ID, keyFile, db)
	if app.WebhookSecret != "" {
		env += "DMC_WEBHOOK_SECRET=" + app.WebhookSecret + "\n"
	}
	return os.WriteFile(envFile, []byte(env), 0o600)
}
