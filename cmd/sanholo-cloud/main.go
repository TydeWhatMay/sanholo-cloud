// Command sanholo-cloud runs the SanHolo Hosted control plane. It is
// configured from the environment only (see internal/config).
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/lhaig/sanholo-cloud/internal/config"
	"github.com/lhaig/sanholo-cloud/internal/db"
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

	app := server.New(pool)
	go func() {
		<-ctx.Done()
		_ = app.Shutdown()
	}()
	if err := app.Listen(cfg.ListenAddr); err != nil {
		log.Fatal(err)
	}
}
