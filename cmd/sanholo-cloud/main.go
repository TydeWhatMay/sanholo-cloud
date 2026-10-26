// Command sanholo-cloud runs the SanHolo Hosted control plane. It is
// configured from the environment only (see internal/config).
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/config"
	"github.com/lhaig/sanholo-cloud/internal/db"
	"github.com/lhaig/sanholo-cloud/internal/mail"
	"github.com/lhaig/sanholo-cloud/internal/server"
)

func main() {
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

	svc := auth.New(pool, auth.Options{
		MaxTenants: cfg.Beta.MaxTenants,
		BaseURL:    cfg.AppBaseURL,
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

	app := server.New(pool, server.WithApp(&server.App{Auth: svc, TrustedProxies: cfg.TrustedProxies}))
	go func() {
		<-ctx.Done()
		_ = app.Shutdown()
	}()
	if err := app.Listen(cfg.ListenAddr); err != nil {
		log.Fatal(err)
	}
}
