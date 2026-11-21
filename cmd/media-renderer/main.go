package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/overmindv/media/internal/render"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := render.Serve(ctx); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
