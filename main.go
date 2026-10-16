// Command dns-guard — DNS-сервис для MikroTik RouterOS: маршрутизация запросов
// по зонам, кэш в памяти, статистика и веб-интерфейс.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/miekg/dns"
	"github.com/nskforward/dns-guard/internal/cache"
	"github.com/nskforward/dns-guard/internal/config"
	"github.com/nskforward/dns-guard/internal/server"
	"github.com/nskforward/dns-guard/internal/stats"
	"github.com/nskforward/dns-guard/internal/web"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "путь к файлу конфигурации")
	logLevel := flag.String("log-level", "info", "уровень логирования: debug|info|warn|error")
	flag.Parse()

	log := newLogger(*logLevel)
	slog.SetDefault(log)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("load config", "path", *cfgPath, "err", err)
		os.Exit(1)
	}

	var c *cache.Cache
	if cfg.Cache.Enabled {
		c = cache.New(cfg.Cache.MaxEntries, cfg.Cache.MinTTL, cfg.Cache.MaxTTL)
	}
	st := stats.New()

	srv, err := server.New(cfg, c, st, log)
	if err != nil {
		log.Error("init dns server", "err", err)
		os.Exit(1)
	}

	app := &application{path: *cfgPath, cfg: cfg, srv: srv, log: log}

	udp := &dns.Server{Addr: cfg.Listen.UDP, Net: "udp", Handler: srv}
	tcp := &dns.Server{Addr: cfg.Listen.TCP, Net: "tcp", Handler: srv}

	errCh := make(chan error, 3)
	go func() {
		log.Info("dns udp listening", "addr", cfg.Listen.UDP)
		if err := udp.ListenAndServe(); err != nil {
			errCh <- err
		}
	}()
	go func() {
		log.Info("dns tcp listening", "addr", cfg.Listen.TCP)
		if err := tcp.ListenAndServe(); err != nil {
			errCh <- err
		}
	}()

	var httpSrv *http.Server
	if cfg.Web.Enabled {
		wsrv := web.New(web.Deps{
			Path:   *cfgPath,
			Stats:  st,
			Cache:  c,
			Config: app.getConfig,
			Reload: app.reload,
			Log:    log,
		})
		httpSrv = &http.Server{
			Addr:              cfg.Web.Listen,
			Handler:           wsrv.Handler(),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			log.Info("web listening", "addr", cfg.Web.Listen)
			if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}

	log.Info("dns-guard started",
		"groups", len(cfg.Groups),
		"rules", len(cfg.Rules),
		"cache", cfg.Cache.Enabled,
	)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	for {
		select {
		case err := <-errCh:
			log.Error("listener failed", "err", err)
			shutdown(udp, tcp, httpSrv, log)
			os.Exit(1)
		case s := <-sig:
			if s == syscall.SIGHUP {
				if err := app.reloadFile(); err != nil {
					log.Error("reload config", "err", err)
				} else {
					log.Info("config reloaded")
				}
				continue
			}
			log.Info("shutting down", "signal", s.String())
			shutdown(udp, tcp, httpSrv, log)
			return
		}
	}
}

// application хранит активную конфигурацию и применяет её изменения.
type application struct {
	mu   sync.RWMutex
	path string
	cfg  *config.Config
	srv  *server.Server
	log  *slog.Logger
}

func (a *application) getConfig() *config.Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg
}

// reload применяет конфигурацию: сначала к DNS-серверу, затем сохраняет в памяти.
func (a *application) reload(cfg *config.Config) error {
	if err := a.srv.Reload(cfg); err != nil {
		return err
	}
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
	return nil
}

func (a *application) reloadFile() error {
	cfg, err := config.Load(a.path)
	if err != nil {
		return err
	}
	return a.reload(cfg)
}

func shutdown(udp, tcp *dns.Server, httpSrv *http.Server, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := udp.ShutdownContext(ctx); err != nil {
		log.Warn("udp shutdown", "err", err)
	}
	if err := tcp.ShutdownContext(ctx); err != nil {
		log.Warn("tcp shutdown", "err", err)
	}
	if httpSrv != nil {
		if err := httpSrv.Shutdown(ctx); err != nil {
			log.Warn("web shutdown", "err", err)
		}
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
