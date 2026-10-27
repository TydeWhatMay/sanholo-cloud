// Command sanholo-cloud runs the SanHolo Hosted control plane. It is
// configured from the environment only (see internal/config).
package main

import (
	"context"
	"log"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lhaig/sanholo-cloud/internal/admin"
	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/config"
	"github.com/lhaig/sanholo-cloud/internal/connect"
	"github.com/lhaig/sanholo-cloud/internal/db"
	"github.com/lhaig/sanholo-cloud/internal/game"
	"github.com/lhaig/sanholo-cloud/internal/mail"
	"github.com/lhaig/sanholo-cloud/internal/nomad"
	"github.com/lhaig/sanholo-cloud/internal/provision"
	"github.com/lhaig/sanholo-cloud/internal/proxy"
	"github.com/lhaig/sanholo-cloud/internal/push"
	"github.com/lhaig/sanholo-cloud/internal/secrets"
	"github.com/lhaig/sanholo-cloud/internal/server"
	"github.com/lhaig/sanholo-cloud/internal/updater"
	"github.com/lhaig/sanholo-cloud/internal/workload"
)

// version is set at build time (-ldflags "-X main.version=1.2.3"); it goes
// into the User-Agent sent to the game.
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "operator" {
		if err := runOperator(context.Background(), os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) > 2 && os.Args[1] == "vapid" && os.Args[2] == "generate" {
		if err := vapidGenerate(os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "release" {
		if err := runRelease(context.Background(), os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		log.Fatal(err)
	}

	// The secrets client is built here so a bad qm address or token file
	// stops the service at start.
	sec, err := secrets.New(secrets.Config{
		Addr: cfg.QM.Addr, RoleID: cfg.QM.RoleID, SecretID: cfg.QM.SecretID, SecretIDFile: cfg.QM.SecretIDFile,
		Token: cfg.QM.Token, TokenFile: cfg.QM.TokenFile,
	})
	if err != nil {
		log.Fatal(err)
	}
	if sec.Enabled() {
		if err := sec.Ready(ctx); err != nil {
			log.Printf("secrets: qm not ready at start: %v", err)
		}
	} else {
		log.Print("secrets: QM_ADDR not set; storing game tokens is disabled")
	}
	gc, err := game.New(game.Config{BaseURL: cfg.GameURL, Version: version})
	if err != nil {
		log.Fatal(err)
	}
	conn, err := connect.New(pool, sec, gc, slog.Default(), cfg.Beta.MaxGameAccountsPerTenant)
	if err != nil {
		log.Fatal(err)
	}

	// Provisioning: without NOMAD_ADDR the service is disabled and tenants
	// stay "not provisioned".
	nc, err := nomad.New(cfg.Nomad.Client)
	if err != nil {
		log.Fatal(err)
	}
	prov, err := provision.New(pool, nc, provision.Options{
		Settings: cfg.Nomad.Job, Volume: cfg.Nomad.Volume, Version: cfg.Nomad.Version, Timeout: cfg.Nomad.Timeout, Logger: slog.Default(),
	})
	if err != nil {
		log.Fatal(err)
	}
	defer prov.Close()
	if prov.Enabled() {
		go prov.Run(ctx, 30*time.Second)
	} else {
		log.Print("provisioning: NOMAD_ADDR not set; tenant bots are not started")
	}

	conn.SetProvisioner(prov)
	go conn.RunCleanups(ctx, time.Minute)

	upd := updater.New(pool, prov, updater.Options{
		BatchSize: cfg.Nomad.RolloutBatch, HealthTimeout: cfg.Nomad.RolloutTimeout,
		CanarySoak: cfg.Nomad.CanarySoak, BatchSoak: cfg.Nomad.BatchSoak, Logger: slog.Default(),
	})
	if prov.Enabled() {
		go upd.Run(ctx, 10*time.Second)
	}

	var wa *webauthn.WebAuthn
	if cfg.WebAuthn.Enabled() {
		wa, err = auth.NewWebAuthn(cfg.WebAuthn.RPID, cfg.WebAuthn.RPOrigin, cfg.WebAuthn.RPName)
		if err != nil {
			log.Fatalf("passkeys: %v", err)
		}
	} else {
		log.Print("passkeys: no relying party (APP_BASE_URL is an IP address); set WEBAUTHN_RP_ID to enable them")
	}
	svc := auth.New(pool, auth.Options{
		MaxTenants: cfg.Beta.MaxTenants,
		BaseURL:    cfg.AppBaseURL,
		WebAuthn:   wa,
	})
	go svc.Run(ctx, 2*time.Second)
	var mailer mail.Mailer = mail.LogSender{}
	if cfg.SMTP.Enabled() {
		mailer = &mail.SMTPSender{
			Host: cfg.SMTP.Host, Port: cfg.SMTP.Port, Username: cfg.SMTP.Username,
			Password: cfg.SMTP.Password, From: cfg.SMTP.From,
		}
	} else {
		log.Print("mail: SMTP_HOST not set; mail is logged as dropped, never sent")
	}
	w := &mail.Worker{Pool: pool, Mailer: mailer}
	go w.Run(ctx, 2*time.Second)

	adm := admin.New(pool, svc)
	adm.SetProvisioner(prov)
	adm.SetUpdater(upd)
	web := &server.App{Auth: svc, Admin: adm, Connect: conn, Provision: prov, TrustedProxies: cfg.TrustedProxies}
	if cfg.ProxyKey != nil {
		appURL, err := url.Parse(cfg.AppBaseURL)
		if err != nil {
			log.Fatalf("proxy: %v", err)
		}
		bridge, err := proxy.New(proxy.Config{
			Key: cfg.ProxyKey, Resolver: prov, Scheme: appURL.Scheme, AppOrigin: cfg.AppBaseURL,
			TrustedProxies: cfg.TrustedProxies, Cookies: server.ControlPlaneCookies(),
		})
		if err != nil {
			log.Fatalf("proxy: %v", err)
		}
		web.Proxy, web.AppBaseURL, web.TenantDomain = bridge, cfg.AppBaseURL, cfg.TenantDomain
	} else {
		log.Print("proxy: PROXY_SIGNING_KEY not set; Bridges are not served and every Host reaches the control plane")
	}
	wirePush(ctx, cfg, pool, web)
	app := server.New(pool, server.WithApp(web))
	go func() {
		<-ctx.Done()
		_ = app.Shutdown()
	}()
	if err := app.Listen(cfg.ListenAddr); err != nil {
		log.Fatal(err)
	}
}

// wirePush turns on Web Push (VAPID keys) and the alert intake (Nomad JWKS).
// Either can be absent in development; the endpoints then answer 503.
func wirePush(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, web *server.App) {
	web.PushStore = push.NewStore(pool)
	if cfg.Push.VAPIDEnabled() {
		domain := cfg.TenantDomain
		if domain == "" {
			domain = "sanholo.space"
		}
		origin, err := server.TenantOriginFunc(cfg.AppBaseURL, domain)
		if err != nil {
			log.Fatalf("push: %v", err)
		}
		web.Push = push.New(push.Config{
			Store: web.PushStore, TenantOrigin: origin, Logger: slog.Default(),
			Sender: push.NewWebPush(push.VAPID{Private: cfg.Push.VAPIDPrivate, Public: cfg.Push.VAPIDPublic, Subject: cfg.Push.VAPIDSubject}),
		})
		web.Push.Start(ctx)
	} else {
		log.Print("push: VAPID keys not set; notifications are not sent")
	}
	if cfg.Push.JWKSURL != "" {
		ns := cfg.Nomad.Client.Namespace
		if ns == "" {
			ns = nomad.DefaultNamespace
		}
		v, err := workload.New(workload.Config{JWKSURL: cfg.Push.JWKSURL, Issuer: cfg.Push.JWTIssuer, Namespace: ns})
		if err != nil {
			log.Fatalf("alerts: %v", err)
		}
		web.Workload = v
	} else {
		log.Print("alerts: no Nomad JWKS URL; /internal/alerts is off")
	}
}
