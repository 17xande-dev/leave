// Command leave is a staff leave application: requests, approvals, balances.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/17xande-dev/websec/csrf"
	"github.com/17xande-dev/websec/session"

	"github.com/17xande-dev/leave/internal/auth"
	"github.com/17xande-dev/leave/internal/config"
	"github.com/17xande-dev/leave/internal/db"
	"github.com/17xande-dev/leave/internal/directory"
	"github.com/17xande-dev/leave/internal/handler"
	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/mcpserver"
	"github.com/17xande-dev/leave/internal/middleware"
	"github.com/17xande-dev/leave/internal/notify"
	"github.com/17xande-dev/leave/internal/oauth"
	"github.com/17xande-dev/leave/internal/outbox"
	"github.com/17xande-dev/leave/internal/users"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "import2026" {
		os.Exit(runImport2026(os.Args[2:], os.Stdout, time.Now))
	}
	migrateStatus := flag.Bool("migrate-status", false, "Print database migration status and exit")
	migrate := flag.Bool("migrate", false, "Apply pending database migrations, print status and exit")
	flag.Parse()
	if *migrate {
		if err := db.Migrate(context.Background(), config.DatabasePath(), os.Stdout); err != nil {
			slog.Error("migrate database", "err", err)
			os.Exit(1)
		}
		return
	}
	if *migrateStatus {
		if err := db.Status(context.Background(), config.DatabasePath(), os.Stdout); err != nil {
			slog.Error("migration status", "err", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, cfg)
}

// serve is the whole program after configuration: it runs until ctx ends or
// the listener fails. Tests call it with a temporary database to prove the
// background jobs, the backup among them, are started.
func serve(ctx context.Context, cfg config.Config) (err error) {
	database, err := db.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	closeDatabase := true
	defer func() {
		if closeDatabase {
			database.Close()
		}
	}()

	dir, err := newDirectory(cfg.Directory)
	if err != nil {
		return err
	}
	userStore := users.NewStore(database)
	// Sessions end after 2 hours unused or 12 hours in all, whichever comes first.
	sessions, err := auth.NewSessions(database, session.Default)
	if err != nil {
		return err
	}
	leaveSvc := leave.New(database, userStore, leave.Config{
		Location: cfg.Location,
	})
	if err := leaveSvc.SeedTypes(context.Background()); err != nil {
		return err
	}
	thisYear := leave.Today(time.Now(), cfg.Location).Year()
	for _, y := range []int{thisYear, thisYear + 1} {
		if err := leaveSvc.EnsureHolidays(context.Background(), y); err != nil {
			return err
		}
	}

	tokens, err := graphTokens(cfg)
	if err != nil {
		return err
	}
	sender, err := newMailer(cfg, tokens)
	if err != nil {
		return err
	}
	calCreate, calDelete := calendarHandlers(newCalendar(cfg, tokens), leaveSvc)
	jobs := outbox.NewStore(database)
	worker := outbox.NewWorker(jobs, map[string]outbox.Handler{
		outbox.KindEmail:          emailHandler(sender),
		outbox.KindCalendarCreate: calCreate,
		outbox.KindCalendarDelete: calDelete,
	})
	leaveSvc.OnEvent(notify.New(cfg.BaseURL).Hook)
	leaveSvc.OnCommit(worker.Kick)

	oauthSrv, mcpHandler, err := assistant(cfg, database, userStore, leaveSvc, jobs)
	if err != nil {
		return err
	}
	app, err := newApp(cfg, handler.Deps{Users: userStore, Sessions: sessions, Directory: dir, Leave: leaveSvc,
		Outbox: jobs, Kick: worker.Kick, TeamsEnabled: teamsSource(cfg.Directory) != "", OAuth: oauthSrv, MCP: mcpHandler})
	if err != nil {
		return err
	}

	backup := backupJob(database, cfg)

	shutdown := startOutbox(ctx, worker, database.Close)
	// The shutdown path owns Close after the worker starts.
	closeDatabase = false
	go every(ctx, cfg.Directory.SyncInterval, func() { syncDirectory(ctx, dir, userStore, teamsSource(cfg.Directory)) })
	go every(ctx, time.Hour, func() {
		if n, err := sessions.DeleteExpired(ctx); err != nil {
			slog.Error("sweep sessions", "err", err)
		} else if n > 0 {
			slog.Info("swept expired sessions", "n", n)
		}
		if n, err := oauthSrv.Sweep(ctx); err != nil {
			slog.Error("sweep oauth", "err", err)
		} else if n > 0 {
			slog.Info("swept expired oauth rows", "n", n)
		}
		// At startup and then hourly: Run takes at most one backup per day
		// and prunes, so the hourly call is a cheap no-op after the first.
		backup(ctx)
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           app,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		// The default is 1 MB per request, and some limiter keys come from a
		// header. Nothing this app sends or sets comes near 64 KiB.
		MaxHeaderBytes: 64 << 10,
		IdleTimeout:    2 * time.Minute,
	}
	defer func() { err = errors.Join(err, shutdown(srv.Shutdown)) }()
	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.ListenAddr, "directory", cfg.Directory.Kind)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	return nil
}

func startOutbox(ctx context.Context, worker *outbox.Worker, closeDB func() error) func(func(context.Context) error) error {
	stop := worker.Start(ctx, 30*time.Second)
	return func(shutdownHTTP func(context.Context) error) error {
		return shutdownService(shutdownHTTP, stop, closeDB)
	}
}

func shutdownService(shutdownHTTP, stopWorker func(context.Context) error, closeDB func() error) error {
	// Drain HTTP and join the worker concurrently on a fresh budget: the
	// signal has already canceled the service context. Leave time for Close
	// and for the process to exit before Kamal's 20-second stop timeout.
	wait, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- stopWorker(wait) }()
	httpErr := shutdownHTTP(wait)
	if errors.Is(httpErr, http.ErrServerClosed) {
		httpErr = nil
	}
	if err := <-stopped; err != nil {
		// An unjoined handler may still need the database.
		return errors.Join(httpErr, fmt.Errorf("stop outbox worker: %w", err))
	}

	waitClose, cancelClose := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelClose()
	closed := make(chan error, 1)
	go func() { closed <- closeDB() }()
	select {
	case err := <-closed:
		return errors.Join(httpErr, err)
	case <-waitClose.Done():
		return errors.Join(httpErr, fmt.Errorf("close database: %w", waitClose.Err()))
	}
}

// assistant builds the AI-assistant side: the OAuth server that signs people
// in and the MCP endpoint that accepts what it issues.
func assistant(cfg config.Config, database *sql.DB, us *users.Store, svc *leave.Service, jobs *outbox.Store) (*oauth.Server, http.Handler, error) {
	source, err := middleware.ClientIPSource(cfg.TrustedProxies)
	if err != nil {
		return nil, nil, err
	}
	o := oauth.New(database, oauth.Config{BaseURL: cfg.BaseURL, ClientIP: source})
	return o, mcpserver.New(mcpserver.Config{Leave: svc, Users: us, Outbox: jobs, Verifier: o.Verify,
		ResourceMetadataURL: o.ResourceMetadataURL(), Scopes: []string{oauth.Scope}}), nil
}

// newApp is the whole HTTP stack: the routes, then CSRF behind a body cap, then the headers and
// the log around everything. Tests build it the same way, so what they hit is
// what is served.
func newApp(cfg config.Config, d handler.Deps) (http.Handler, error) {
	source, err := middleware.ClientIPSource(cfg.TrustedProxies)
	if err != nil {
		return nil, err
	}
	protect, err := newCSRF(cfg)
	if err != nil {
		return nil, err
	}
	h, err := handler.New(handler.Config{
		CSRF:         protect,
		CookieSecure: cfg.CookieSecure,
		ClientIP:     source,
	}, d)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return middleware.Logging(middleware.SecurityHeaders(protect.Handler(mux))), nil
}

func newDirectory(c config.Directory) (directory.Directory, error) {
	if c.Kind == "fake" {
		slog.Warn("DIRECTORY=fake: any password signs in as any listed user; development only", "file", c.FakeFile)
		return directory.LoadFake(c.FakeFile)
	}
	return directory.NewLDAP(directory.LDAPConfig{
		URL: c.URL, BindDN: c.BindDN, BindPassword: c.BindPassword, BaseDN: c.BaseDN,
		UserFilter: c.UserFilter, AdminGroup: c.AdminGroup, TeamsBaseDN: c.TeamsBaseDN, CAFile: c.CAFile,
		InsecureSkipVerify: c.InsecureSkipVerify,
	})
}

// teamsSource names where teams come from ("ldap", "fake"), or is empty when
// the feature is off: an LDAP directory with no LDAP_TEAMS_BASE_DN.
func teamsSource(c config.Directory) string {
	switch {
	case c.Kind == "fake":
		return "fake"
	case c.TeamsBaseDN != "":
		return "ldap"
	}
	return ""
}

// syncDirectory refreshes the local users copy, then the teams (which refer
// to users). A failure is logged and the old copy kept: the app keeps working
// on slightly stale data rather than going down with the DC. teams is the
// teams source's name, empty when teams are off.
func syncDirectory(ctx context.Context, dir directory.Directory, us *users.Store, teams string) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	all, err := dir.AllUsers(ctx)
	if err != nil {
		slog.Error("directory sync", "err", err)
		return
	}
	n, disabled, err := us.Sync(ctx, all)
	if err != nil {
		slog.Error("directory sync", "err", err)
		return
	}
	slog.Info("directory sync", "users", n, "disabled", disabled)

	ts, ok := dir.(directory.TeamSource)
	if teams == "" || !ok {
		return
	}
	list, err := ts.Teams(ctx)
	if err != nil {
		// Not applied: a failed search must not look like "no teams", which
		// would empty every team.
		slog.Error("team sync", "err", err)
		return
	}
	n, gone, err := us.SyncTeams(ctx, teams, list)
	if err != nil {
		slog.Error("team sync", "err", err)
		return
	}
	slog.Info("team sync", "teams", n, "disabled", gone)
}

// every runs f now and then on each tick until ctx ends.
func every(ctx context.Context, d time.Duration, f func()) {
	f()
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f()
		}
	}
}

// csrfExemptPaths are the endpoints that skip the token check, because their
// callers are programs with no cookie to ride: /mcp takes a bearer token, the
// OAuth endpoints take their own secrets (a code with its PKCE verifier, a
// refresh token) or nothing at all (registration), and the metadata is public.
// A cross-site page cannot make a browser send an Authorization header or a
// code verifier it does not have, which is all the token check defends against.
// /oauth/authorize is deliberately absent: it is a form in a signed-in browser
// and keeps its CSRF token. TestCSRFExemptions pins this list.
//
// The match is on the exact path, so any other spelling of one of these is not
// exempt: it fails closed, into the token check.
var csrfExemptPaths = []string{
	"/mcp",
	"/oauth/token",
	"/oauth/register",
	"/oauth/revoke",
	"/.well-known/oauth-authorization-server",
	"/.well-known/oauth-protected-resource",
	"/.well-known/oauth-protected-resource/mcp",
}

// newCSRF builds the CSRF policy: the token and same-origin check, with the
// body cap in front of it (nosurf parses the form to find the token, so the cap
// has to come first). BASE_URL is the app's own origin; an empty or malformed
// one is a startup error. Behind a TLS-terminating proxy r.TLS is nil, so
// COOKIE_SECURE is the honest signal for "the browser is on https".
func newCSRF(cfg config.Config) (*csrf.Protect, error) {
	return csrf.New(csrf.Config{
		Origin:   cfg.BaseURL,
		Insecure: !cfg.CookieSecure,
		MaxBody:  middleware.MaxFormBytes,
		Exempt:   func(r *http.Request) bool { return slices.Contains(csrfExemptPaths, r.URL.Path) },
	})
}
