// Command leave is a staff leave application: requests, approvals, balances.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/justinas/nosurf"

	"github.com/17xande-dev/leave/internal/auth"
	"github.com/17xande-dev/leave/internal/config"
	"github.com/17xande-dev/leave/internal/db"
	"github.com/17xande-dev/leave/internal/directory"
	"github.com/17xande-dev/leave/internal/handler"
	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/middleware"
	"github.com/17xande-dev/leave/internal/notify"
	"github.com/17xande-dev/leave/internal/outbox"
	"github.com/17xande-dev/leave/internal/users"
)

func main() {
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
	database, err := db.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer database.Close()

	dir, err := newDirectory(cfg.Directory)
	if err != nil {
		return err
	}
	userStore := users.NewStore(database)
	sessions := auth.NewSessions(database, cfg.SessionTTL)
	leaveSvc := leave.New(database, userStore, leave.Config{
		Location: cfg.Location, FinalApprover: cfg.FinalApprover,
		SickCycleAnchor: cfg.SickCycleStartYear, SickBackdateDays: cfg.SickBackdateDays,
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

	h, err := handler.New(handler.Config{
		CookieSecure:   cfg.CookieSecure,
		SessionTTL:     cfg.SessionTTL,
		TrustedProxies: middleware.ParseTrustedProxies(cfg.TrustedProxies),
	}, handler.Deps{Users: userStore, Sessions: sessions, Directory: dir, Leave: leaveSvc,
		Outbox: jobs, Kick: worker.Kick})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go worker.Run(ctx, 30*time.Second)
	go every(ctx, cfg.Directory.SyncInterval, func() { syncDirectory(ctx, dir, userStore) })
	go every(ctx, time.Hour, func() {
		if n, err := sessions.DeleteExpired(ctx); err != nil {
			slog.Error("sweep sessions", "err", err)
		} else if n > 0 {
			slog.Info("swept expired sessions", "n", n)
		}
	})

	mux := http.NewServeMux()
	h.Routes(mux)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           middleware.Logging(middleware.SecurityHeaders(csrf(mux, cfg))),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func newDirectory(c config.Directory) (directory.Directory, error) {
	if c.Kind == "fake" {
		slog.Warn("DIRECTORY=fake: any password signs in as any listed user; development only", "file", c.FakeFile)
		return directory.LoadFake(c.FakeFile)
	}
	return directory.NewLDAP(directory.LDAPConfig{
		URL: c.URL, BindDN: c.BindDN, BindPassword: c.BindPassword, BaseDN: c.BaseDN,
		UserFilter: c.UserFilter, AdminGroup: c.AdminGroup, CAFile: c.CAFile,
		InsecureSkipVerify: c.InsecureSkipVerify,
	})
}

// syncDirectory refreshes the local users copy. A failure is logged and the
// old copy kept: the app keeps working on slightly stale data rather than
// going down with the DC.
func syncDirectory(ctx context.Context, dir directory.Directory, us *users.Store) {
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

// csrf wraps the mux in nosurf's token check plus its same-origin check.
func csrf(next http.Handler, cfg config.Config) http.Handler {
	n := nosurf.New(next)
	n.SetBaseCookie(http.Cookie{Path: "/", HttpOnly: true, Secure: cfg.CookieSecure, SameSite: http.SameSiteLaxMode})
	n.SetFailureHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slog.Warn("csrf refused", "path", r.URL.Path, "reason", nosurf.Reason(r))
		http.Error(w, "This form has expired. Go back, reload the page and try again.", http.StatusForbidden)
	}))
	// nosurf compares Origin with the request's Host assuming https. Behind a
	// TLS-terminating proxy r.TLS is nil, so CookieSecure is the honest signal
	// for "the browser is on https".
	n.SetIsTLSFunc(func(*http.Request) bool { return cfg.CookieSecure })
	if u, err := url.Parse(cfg.BaseURL); err == nil && u.Host != "" {
		own := u.Scheme + "://" + u.Host
		n.SetIsAllowedOriginFunc(func(o *url.URL) bool { return o.Scheme+"://"+o.Host == own })
	}
	return n
}
