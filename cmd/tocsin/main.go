package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/alexedwards/argon2id"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/thomas-maurice/tocsin/gen/notifier/v1/notifierv1connect"
	"github.com/thomas-maurice/tocsin/internal/api"
	"github.com/thomas-maurice/tocsin/internal/chart"
	"github.com/thomas-maurice/tocsin/internal/config"
	"github.com/thomas-maurice/tocsin/internal/logging"
	"github.com/thomas-maurice/tocsin/internal/matrix"
	"github.com/thomas-maurice/tocsin/internal/outbox"
	"github.com/thomas-maurice/tocsin/internal/server"
	"github.com/thomas-maurice/tocsin/internal/store"
	"github.com/thomas-maurice/tocsin/ui"
)

func main() {
	var configPath string
	var resetIdentity bool
	rootCmd := &cobra.Command{
		Use:           "tocsin",
		Short:         "HTTP notification gateway (Gotify + Alertmanager compatible) that delivers to encrypted Matrix rooms",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(configPath, resetIdentity)
		},
	}
	rootCmd.Flags().StringVarP(&configPath, "config", "c", "", "path to config file (default: ./config.yaml)")
	rootCmd.Flags().BoolVar(&resetIdentity, "reset-identity", false, "log out all other devices, replace the account's cross-signing keys and generate a fresh recovery key (use when the recovery key is lost or the account may be compromised)")

	tokenCmd := &cobra.Command{Use: "token", Short: "Token utilities"}
	tokenCmd.AddCommand(&cobra.Command{
		Use:   "hash [token]",
		Short: "Print the argon2id hash of an admin token (reads stdin if no argument)",
		Long:  "Hash an admin API token for the admin_token_hash config key / TOCSIN_ADMIN_TOKEN_HASH env var. Prefer piping via stdin so the token stays out of shell history.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return hashToken(cmd.OutOrStdout(), cmd.InOrStdin(), args)
		},
	})
	rootCmd.AddCommand(tokenCmd)
	rootCmd.AddCommand(newSendCmd())
	rootCmd.AddCommand(newVerifyIdentityCmd())
	rootCmd.AddCommand(newUnwedgeCmd())

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func hashToken(out io.Writer, in io.Reader, args []string) error {
	var token string
	switch {
	case len(args) == 1:
		token = args[0]
	case term.IsTerminal(int(os.Stdin.Fd())):
		// Interactive: prompt without echoing the token, return on Enter.
		fmt.Fprint(os.Stderr, "Token: ")
		raw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return fmt.Errorf("reading token: %w", err)
		}
		token = strings.TrimSpace(string(raw))
	default:
		// Piped: first line is the token, no Ctrl-D required.
		raw, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && err != io.EOF {
			return fmt.Errorf("reading token from stdin: %w", err)
		}
		token = strings.TrimSpace(raw)
	}
	if token == "" {
		return errors.New("empty token")
	}
	hash, err := argon2id.CreateHash(token, argon2id.DefaultParams)
	if err != nil {
		return fmt.Errorf("hashing token: %w", err)
	}
	fmt.Fprintln(out, hash)
	return nil
}

func resolveAliasChannels(ctx context.Context, log *slog.Logger, st *store.Store, bot *matrix.Bot) {
	channels, err := st.ListChannels(ctx)
	if err != nil {
		log.Warn("could not list channels for alias resolution", "error", err)
		return
	}
	for _, ch := range channels {
		if !strings.HasPrefix(ch.RoomID, "#") {
			continue
		}
		roomID, err := bot.ResolveRoom(ctx, ch.RoomID)
		if err != nil {
			log.Warn("channel has an unresolvable room alias; it cannot deliver", "channel", ch.Name, "alias", ch.RoomID, "error", err)
			continue
		}
		if err := st.UpdateChannelRoomID(ctx, ch.Name, roomID); err != nil {
			log.Warn("failed to update alias channel", "channel", ch.Name, "error", err)
			continue
		}
		log.Info("resolved channel room alias", "channel", ch.Name, "alias", ch.RoomID, "room_id", roomID)
	}
}

func run(configPath string, resetIdentity bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	cfg.ResetIdentity = resetIdentity
	level, err := logging.ParseLevel(cfg.LogLevel)
	if err != nil {
		return err
	}
	log := logging.New(level)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx = logging.Into(ctx, log)

	st, err := store.Open(cfg.Database)
	if err != nil {
		return err
	}

	bot, err := matrix.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer bot.Close()
	if err := bot.Start(ctx); err != nil {
		return err
	}

	// Channels created before alias support (or edited by hand) may hold a
	// room alias; every internal lookup is ID-keyed, so resolve them once.
	resolveAliasChannels(ctx, log, st, bot)

	// The config hash only seeds the credential on first boot; afterwards
	// the database row (managed via ChangeAdminPassword) is authoritative.
	auth, err := api.NewAdminAuth(ctx, st, cfg.AdminTokenHash)
	if err != nil {
		return err
	}
	var charts *chart.Client
	if cfg.PrometheusURL != "" {
		charts = chart.New(cfg.PrometheusURL)
		log.Info("chart rendering enabled", "prometheus_url", cfg.PrometheusURL)
	}
	// The dispatcher drains the durable outbox: ingest handlers only queue,
	// so webhooks answer fast and nothing is lost while Matrix is down.
	dispatcher := outbox.New(log, st, bot, charts, time.Duration(cfg.OutboxRetentionHours)*time.Hour)
	go dispatcher.Run(ctx)

	adminPath, adminHandler := notifierv1connect.NewAdminServiceHandler(
		api.NewServer(st, bot, auth, cfg.Database.Type, dispatcher.Kick),
		connect.WithInterceptors(auth.Interceptor()),
	)

	rl := server.NewLimiters(cfg.RateLimitPerSecond, cfg.RateLimitBurst)
	ingest := server.New(log, bot, st, dispatcher, rl)
	mux := http.NewServeMux()
	mux.Handle(adminPath, adminHandler)
	for _, p := range []string{"/message", "/alertmanager", "/gitea", "/forgejo", "/slack", "/grafana", "/health", "/version", "/metrics"} {
		mux.Handle(p, ingest)
	}
	mux.Handle("/", ui.Handler())

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: mux,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case err := <-bot.SyncErr():
		// Fatal sync death (e.g. revoked token): die loud, let the
		// supervisor restart us — a fresh start logs in again and recovers.
		return fmt.Errorf("matrix sync died: %w", err)
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
