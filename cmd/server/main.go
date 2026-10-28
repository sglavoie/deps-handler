// Command server is the hosted deps-merger GitHub App: it takes webhook
// deliveries and a schedule, runs deps-merger's engine on each repository
// the App is installed on, and keeps a "Dependency health" issue in each.
// It merges only with DMC_ALLOW_MERGE, in repositories whose policy says
// mode: merge, for installations not paused (cmd/admin pause); it never
// comments.
//
// Configured by the environment:
//
//	DMC_APP_ID             the App's ID
//	DMC_PRIVATE_KEY        its private key (PEM), or
//	DMC_PRIVATE_KEY_FILE   a file holding it
//	DMC_WEBHOOK_SECRET     the webhook secret (required unless -once)
//	DMC_DB                 the SQLite database (default dmc.db)
//	PORT                   the port to listen on (default 8080)
//	DMC_HOST               the address to listen on (default every one), such
//	                       as 127.0.0.1 behind a tunnel on the same machine
//	DMC_SWEEP              how often every repository runs (default 4h)
//	DMC_DEBOUNCE           how long an event waits for others (default 1m)
//	DMC_ALLOW_MERGE        true to let it merge where mode: merge is set
//	                       (default false: report only)
//	DMC_REVIEW_URL         an OpenAI-compatible chat completions API, such
//	                       as a local llama-server's http://127.0.0.1:8081/v1:
//	                       with DMC_REVIEW_MODEL, turns on agent review for
//	                       repositories with agent_review: true
//	DMC_REVIEW_AGENT       api (default), claude or codex; the CLI agents use
//	                       their local login and need no review URL or API key
//	DMC_REVIEW_MODEL       the model, as the API names it
//	DMC_REVIEW_KEY_ENV     the variable holding the API's key, if it wants one
//	DMC_REVIEW_TOOLS       false to let the model judge from the prompt alone
//	DMC_REVIEWS_PER_DAY    each installation's reviews over 24 hours (default 20)
//	DMC_REVIEW_TIMEOUT     one review's limit (default 20m)
//	DMC_REPOS              if set, the only repositories it runs, as
//	                       owner/name separated by commas, whatever the
//	                       installations reach
//
// With -once it runs the repositories named, or with -all every one, once
// and exits, without serving.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sglavoie/deps-merger/internal/app"
	"github.com/sglavoie/deps-merger/internal/jobs"
	"github.com/sglavoie/deps-merger/internal/service"
	"github.com/sglavoie/deps-merger/internal/store"
	"github.com/sglavoie/deps-merger/internal/webhook"
	"github.com/sglavoie/deps-merger/pkg/review"
)

func main() {
	once := flag.Bool("once", false, "run the repositories named once, and exit")
	all := flag.Bool("all", false, "with -once, run every repository the App can reach")
	flag.Parse()
	if *once && *all == (flag.NArg() > 0) {
		fmt.Fprintln(os.Stderr, "server: -once needs repositories named as owner/name, or -all")
		os.Exit(2)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log, *once, flag.Args()); err != nil {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger, once bool, only []string) error {
	cfg, err := loadEnv(once)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.db)
	if err != nil {
		return err
	}
	defer st.Close()
	a, err := app.New(ctx, cfg.appID, cfg.key)
	if err != nil {
		return err
	}
	if !slices.Contains(a.Events, "status") {
		log.Warn("GitHub App is not subscribed to commit status events; enable the status event in its settings so status-only CI can trigger runs", "app", a.Slug)
	}
	log.Info("started", "app", a.Slug, "db", cfg.db, "allow_merge", cfg.allowMerge, "repos", cfg.repos)
	s := &service.Service{Platform: a, Store: st, Log: log, Allowed: cfg.repos, AllowMerge: cfg.allowMerge, Review: cfg.review}
	if cfg.review != nil {
		log.Info("agent review on", "model", cfg.review.Model, "per_day", cfg.review.PerDay)
	}

	if once {
		return s.Once(ctx, only)
	}

	workCtx, stopWork := context.WithCancel(ctx)
	defer stopWork()
	q := &jobs.Queues{Run: s.Run, Debounce: cfg.debounce}
	q.Start(workCtx)
	mux := http.NewServeMux()
	mux.Handle("/webhook", &webhook.Handler{
		Secret:  cfg.secret,
		Enqueue: q.Enqueue,
		Installed: func(ctx context.Context, id int64, account string) error {
			return s.InstallationAdded(ctx, id, account)
		},
		Uninstalled: func(ctx context.Context, id int64, deleted bool) error {
			if err := s.InstallationRemoved(ctx, id, deleted); err != nil {
				return err
			}
			if deleted {
				log.Info("installation forgotten", "installation", id)
			}
			return nil
		},
		Log: log.With("app", a.Slug, "app_id", a.ID),
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	srv := &http.Server{Addr: net.JoinHostPort(cfg.host, cfg.port), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-workCtx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	go s.SweepEvery(workCtx, cfg.sweep, q.Enqueue)

	log.Info("listening", "port", cfg.port)
	err = listenAndDrain(srv, q, stopWork)
	<-shutdownDone
	return err
}

// listenAndDrain always stops the workers when the listener exits, including
// startup failures such as an occupied port.
func listenAndDrain(srv *http.Server, q *jobs.Queues, stop context.CancelFunc) error {
	err := srv.ListenAndServe()
	stop()
	q.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type env struct {
	review          *service.Review
	allowMerge      bool
	repos           []string
	appID           int64
	key, secret     []byte
	db, host, port  string
	sweep, debounce time.Duration
}

func loadEnv(once bool) (env, error) {
	e := env{db: or(os.Getenv("DMC_DB"), "dmc.db"), host: os.Getenv("DMC_HOST"), port: or(os.Getenv("PORT"), "8080"), secret: []byte(os.Getenv("DMC_WEBHOOK_SECRET"))}
	var err error
	if e.appID, err = strconv.ParseInt(os.Getenv("DMC_APP_ID"), 10, 64); err != nil {
		return e, fmt.Errorf("DMC_APP_ID: %w", err)
	}
	switch k, f := os.Getenv("DMC_PRIVATE_KEY"), os.Getenv("DMC_PRIVATE_KEY_FILE"); {
	case k != "":
		e.key = []byte(k)
	case f != "":
		if e.key, err = os.ReadFile(f); err != nil {
			return e, err
		}
	default:
		return e, errors.New("set DMC_PRIVATE_KEY or DMC_PRIVATE_KEY_FILE")
	}
	if len(e.secret) == 0 && !once {
		return e, errors.New("DMC_WEBHOOK_SECRET is not set: deliveries could not be verified")
	}
	if e.sweep, err = time.ParseDuration(or(os.Getenv("DMC_SWEEP"), "4h")); err != nil || e.sweep < time.Minute {
		return e, fmt.Errorf("DMC_SWEEP: want a duration of at least 1m")
	}
	if e.debounce, err = time.ParseDuration(or(os.Getenv("DMC_DEBOUNCE"), "1m")); err != nil || e.debounce < 0 {
		return e, fmt.Errorf("DMC_DEBOUNCE: want a duration")
	}
	if v := os.Getenv("DMC_ALLOW_MERGE"); v != "" {
		if e.allowMerge, err = strconv.ParseBool(v); err != nil {
			return e, fmt.Errorf("DMC_ALLOW_MERGE: want true or false")
		}
	}
	if e.review, err = loadReview(e.db); err != nil {
		return e, err
	}
	for r := range strings.SplitSeq(os.Getenv("DMC_REPOS"), ",") {
		if r = strings.TrimSpace(r); r != "" {
			if o, n, ok := strings.Cut(r, "/"); !ok || o == "" || n == "" {
				return e, fmt.Errorf("DMC_REPOS: %q is not owner/name", r)
			}
			e.repos = append(e.repos, r)
		}
	}
	return e, nil
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// loadReview reads the agent review settings. The API backend needs an
// endpoint; CLI backends use their local login. Checkouts and verdicts go
// next to the database.
func loadReview(db string) (*service.Review, error) {
	backend := or(os.Getenv("DMC_REVIEW_AGENT"), "api")
	model := backend
	var agent func(func(review.Usage)) review.Agent
	var available func(context.Context) error
	switch backend {
	case "api":
		url := os.Getenv("DMC_REVIEW_URL")
		model = os.Getenv("DMC_REVIEW_MODEL")
		if url == "" && model == "" {
			return nil, nil
		}
		if url == "" || model == "" {
			return nil, errors.New("agent review needs both DMC_REVIEW_URL and DMC_REVIEW_MODEL")
		}
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return nil, fmt.Errorf("DMC_REVIEW_URL %q is not an http or https URL", url)
		}
		key := ""
		if name := os.Getenv("DMC_REVIEW_KEY_ENV"); name != "" {
			if key = os.Getenv(name); key == "" {
				return nil, fmt.Errorf("DMC_REVIEW_KEY_ENV names %s, which is not set", name)
			}
		}
		tools := true
		if v := os.Getenv("DMC_REVIEW_TOOLS"); v != "" {
			var err error
			if tools, err = strconv.ParseBool(v); err != nil {
				return nil, errors.New("DMC_REVIEW_TOOLS: want true or false")
			}
		}
		agent = func(usage func(review.Usage)) review.Agent {
			return review.API{URL: url, Model: model, Key: key, Tools: tools, Usage: usage}
		}
		available = func(ctx context.Context) error { return ping(ctx, url, key) }
	case "claude":
		agent = func(func(review.Usage)) review.Agent { return review.Claude{} }
	case "codex":
		agent = func(func(review.Usage)) review.Agent { return review.Codex{} }
	default:
		return nil, fmt.Errorf("DMC_REVIEW_AGENT: %q (want api, claude or codex)", backend)
	}
	if backend != "api" {
		if _, err := exec.LookPath(backend); err != nil {
			return nil, fmt.Errorf("DMC_REVIEW_AGENT: %s is not installed or not on PATH", backend)
		}
	}
	perDay := 20
	if v := os.Getenv("DMC_REVIEWS_PER_DAY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, errors.New("DMC_REVIEWS_PER_DAY: want a number of at least 1")
		}
		perDay = n
	}
	timeout, err := time.ParseDuration(or(os.Getenv("DMC_REVIEW_TIMEOUT"), "20m"))
	if err != nil || timeout < time.Minute {
		return nil, errors.New("DMC_REVIEW_TIMEOUT: want a duration of at least 1m")
	}
	dir := strings.TrimSuffix(db, filepath.Ext(db)) + "-reviews"
	return &service.Review{
		Agent: agent,
		Model: model, PerDay: perDay, Timeout: timeout, Dir: dir,
		Available: available,
	}, nil
}

// ping asks the model's API for its models, which a server that is up
// answers at once.
func ping(ctx context.Context, url, key string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(url, "/")+"/models", nil)
	if err != nil {
		return err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s/models: %s", url, resp.Status)
	}
	return nil
}
