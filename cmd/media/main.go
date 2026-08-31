// media — сервис медиафайлов Overmindv.
package main

import (
	"os"

	"github.com/overmindv/parker"

	"github.com/overmindv/media/internal/app/media"
)

// main запускает Media через каркас parker.
func main() {
	os.Exit(parker.Main(run, parker.WithAppName("media")))
}

// run регистрирует бизнес-логику на каркас parker.
func run(app *parker.App) error {
	return media.Build(app)
}
