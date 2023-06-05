package redis

import (
	lediscfg "github.com/ledisdb/ledisdb/config"
	"github.com/ledisdb/ledisdb/server"
)

func NewLedisServer(url string) (*server.App, error) {
	cfg := lediscfg.NewConfigDefault()
	cfg.Addr = url
	var app *server.App
	app, err := server.NewApp(cfg)
	if err != nil {
		return nil, err
	}
	return app, nil
}
