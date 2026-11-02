// CloudBox — self-hosted облачное хранилище в духе Nextcloud Files.
//
//	cloudbox                       — запустить сервер
//	cloudbox useradd [-admin] NAME PASSWORD
//	cloudbox passwd NAME PASSWORD
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/andrk774/cloudbox/internal/api"
	"github.com/andrk774/cloudbox/internal/auth"
	"github.com/andrk774/cloudbox/internal/config"
	"github.com/andrk774/cloudbox/internal/dav"
	"github.com/andrk774/cloudbox/internal/db"
	"github.com/andrk774/cloudbox/internal/files"
	"github.com/andrk774/cloudbox/internal/storage"
	"github.com/andrk774/cloudbox/web"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("cloudbox: ")
	cfg := config.Load()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	database, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer database.Close()

	authSvc := &auth.Service{DB: database, DefaultQuota: cfg.DefaultQuotaMB << 20}

	if len(os.Args) > 1 {
		if err := runCommand(ctx, authSvc, os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "ошибка:", err)
			os.Exit(1)
		}
		return
	}

	var st storage.Storage
	switch cfg.Storage {
	case "local":
		st, err = storage.NewLocal(cfg.DataDir)
		log.Printf("хранилище: локальный диск %s", cfg.DataDir)
	case "s3":
		var s3 *storage.S3
		s3, err = storage.NewS3(cfg.S3Endpoint, cfg.S3Region, cfg.S3Bucket, cfg.S3Access, cfg.S3Secret, cfg.S3PathStyle)
		if err == nil {
			err = s3.Check(ctx)
		}
		st = s3
		log.Printf("хранилище: S3 %s, бакет %s", cfg.S3Endpoint, cfg.S3Bucket)
	default:
		err = fmt.Errorf("CB_STORAGE должен быть local или s3, получено %q", cfg.Storage)
	}
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(cfg.TmpDir, 0o750); err != nil {
		log.Fatal(err)
	}

	fileSvc := &files.Service{DB: database, St: st, TmpDir: cfg.TmpDir, MaxVersions: cfg.MaxVersions, MaxUpload: cfg.MaxUploadMB << 20}
	srv := &api.Server{
		Cfg: cfg, Auth: authSvc, Files: fileSvc, Web: web.FS(),
		DAV: &dav.Handler{Auth: authSvc, Files: fileSvc, TmpDir: cfg.TmpDir},
	}

	go background(ctx, cfg, authSvc, fileSvc)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		// ReadTimeout/WriteTimeout не задаём: большие загрузки и скачивания могут идти часами
	}
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		httpSrv.Shutdown(shCtx)
	}()
	if n, _ := authSvc.CountUsers(ctx); n == 0 {
		log.Printf("пользователей нет — откройте %s и зарегистрируйте администратора", cfg.PublicURL)
	}
	log.Printf("слушаю %s (WebDAV: %s/dav/)", cfg.Listen, cfg.PublicURL)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// background — периодические задачи: чистка корзины, удаление неиспользуемых блобов, старых сессий.
func background(ctx context.Context, cfg config.Config, a *auth.Service, f *files.Service) {
	f.GC(ctx)
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.PurgeExpired(ctx, cfg.TrashDays)
			f.GC(ctx)
			a.Cleanup(ctx)
		}
	}
}

func runCommand(ctx context.Context, a *auth.Service, args []string) error {
	switch args[0] {
	case "useradd":
		fs := flag.NewFlagSet("useradd", flag.ExitOnError)
		admin := fs.Bool("admin", false, "сделать администратором")
		quota := fs.Int64("quota-mb", -1, "квота в МБ (0 — без ограничения, по умолчанию CB_DEFAULT_QUOTA_MB)")
		fs.Parse(args[1:])
		if fs.NArg() != 2 {
			return errors.New("использование: cloudbox useradd [-admin] [-quota-mb N] ИМЯ ПАРОЛЬ")
		}
		q := *quota
		if q > 0 {
			q <<= 20
		}
		u, err := a.CreateUser(ctx, fs.Arg(0), fs.Arg(1), *admin, q)
		if err != nil {
			return err
		}
		fmt.Printf("создан пользователь %s (id %d, admin=%v)\n", u.Username, u.ID, u.IsAdmin)
	case "passwd":
		if len(args) != 3 {
			return errors.New("использование: cloudbox passwd ИМЯ ПАРОЛЬ")
		}
		users, err := a.ListUsers(ctx)
		if err != nil {
			return err
		}
		for _, u := range users {
			if u.Username == args[1] {
				if err := a.SetPassword(ctx, u.ID, args[2]); err != nil {
					return err
				}
				fmt.Println("пароль изменён")
				return nil
			}
		}
		return errors.New("пользователь не найден")
	default:
		return fmt.Errorf("неизвестная команда %q (есть: useradd, passwd)", args[0])
	}
	return nil
}
