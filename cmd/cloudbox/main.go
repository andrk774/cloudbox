// CloudBox — self-hosted облачное хранилище в духе Nextcloud.
//
//	cloudbox                                 — запустить сервер
//	cloudbox useradd [-admin] [-quota-mb N] ИМЯ ПАРОЛЬ
//	cloudbox passwd ИМЯ ПАРОЛЬ
//	cloudbox backup -o ФАЙЛ.tar.gz           — бэкап БД и файлов
//	cloudbox restore -i ФАЙЛ.tar.gz          — восстановление в пустую базу
//	cloudbox migrate-storage -to s3|local    — перенос файлов между хранилищами
//	cloudbox reindex                         — переиндексировать содержимое для поиска
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/andrk774/cloudbox/internal/api"
	"github.com/andrk774/cloudbox/internal/auth"
	"github.com/andrk774/cloudbox/internal/backup"
	"github.com/andrk774/cloudbox/internal/config"
	"github.com/andrk774/cloudbox/internal/dav"
	"github.com/andrk774/cloudbox/internal/db"
	"github.com/andrk774/cloudbox/internal/files"
	"github.com/andrk774/cloudbox/internal/oidc"
	"github.com/andrk774/cloudbox/internal/pim"
	"github.com/andrk774/cloudbox/internal/storage"
	"github.com/andrk774/cloudbox/internal/wopi"
	"github.com/andrk774/cloudbox/web"
)

func main() {
	cfg := config.Load()
	setupLogging(cfg.LogFormat)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		slog.Warn("неизвестный часовой пояс, использую UTC", "tz", cfg.Timezone)
		loc = time.UTC
	}

	// restore работает с пустой базой — до миграций
	if len(os.Args) > 1 && os.Args[1] == "restore" {
		if err := cmdRestore(ctx, cfg, os.Args[2:]); err != nil {
			fatal(err)
		}
		return
	}

	database, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		fatal(err)
	}
	defer database.Close()

	authSvc := &auth.Service{DB: database, DefaultQuota: cfg.DefaultQuotaMB << 20, Secret: cfg.Secret}

	if len(os.Args) > 1 {
		if err := runCommand(ctx, cfg, database, authSvc, loc, os.Args[1:]); err != nil {
			fatal(err)
		}
		return
	}

	st, err := openStorage(ctx, cfg, cfg.Storage)
	if err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(cfg.TmpDir, 0o750); err != nil {
		fatal(err)
	}

	fileSvc := &files.Service{DB: database, St: st, TmpDir: cfg.TmpDir, MaxVersions: cfg.MaxVersions,
		MaxUpload: cfg.MaxUploadMB << 20, Loc: loc}
	pimSvc := &pim.Service{DB: database, Loc: loc}
	srv := &api.Server{
		Cfg: cfg, DB: database, Auth: authSvc, Files: fileSvc, PIM: pimSvc, Web: web.FS(),
		DAV:    &dav.Handler{Auth: authSvc, Files: fileSvc, TmpDir: cfg.TmpDir},
		PIMDAV: &pim.Handler{Auth: authSvc, Svc: pimSvc},
		WOPI: &wopi.Server{Files: fileSvc, Cfg: wopi.Config{EditorURL: cfg.WOPIEditorURL, DiscoveryURL: cfg.WOPIDiscoveryURL,
			HostURL: cfg.WOPIHostURL, Secret: cfg.Secret}},
	}
	if cfg.OIDCIssuer != "" {
		srv.OIDC = oidc.New(oidc.Config{Issuer: cfg.OIDCIssuer, ClientID: cfg.OIDCClientID, ClientSecret: cfg.OIDCClientSecret,
			RedirectURL: cfg.PublicURL + "/api/auth/oidc/callback"})
		slog.Info("вход через OIDC включён", "issuer", cfg.OIDCIssuer)
	}
	if cfg.WOPIEditorURL != "" {
		slog.Info("онлайн-редактор", "url", cfg.WOPIEditorURL)
	}

	go background(ctx, cfg, authSvc, fileSvc)
	go fileSvc.RunIndexer(ctx)
	go fileSvc.BackfillMedia(ctx)
	go backup.Scheduled(ctx, cfg.DatabaseURL, database, st, cfg.BackupDir, cfg.BackupHour, cfg.BackupKeep, cfg.TmpDir)

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
		slog.Info("пользователей нет — откройте адрес и зарегистрируйте администратора", "url", cfg.PublicURL)
	}
	slog.Info("CloudBox запущен", "listen", cfg.Listen, "webdav", cfg.PublicURL+"/dav/", "caldav", cfg.PublicURL+"/caldav/",
		"encryption", cfg.EncryptionKey != "")
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal(err)
	}
}

func setupLogging(format string) {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	var h slog.Handler
	if format == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h)) // log.Printf тоже пойдёт через slog
	log.SetFlags(0)
}

func fatal(err error) {
	slog.Error(err.Error())
	fmt.Fprintln(os.Stderr, "ошибка:", err)
	os.Exit(1)
}

// openStorage открывает хранилище нужного вида (с шифрованием, если задан ключ).
func openStorage(ctx context.Context, cfg config.Config, kind string) (storage.Storage, error) {
	var st storage.Storage
	switch kind {
	case "local":
		l, err := storage.NewLocal(cfg.DataDir)
		if err != nil {
			return nil, err
		}
		st = l
		slog.Info("хранилище: локальный диск", "dir", cfg.DataDir)
	case "s3":
		s3, err := storage.NewS3(cfg.S3Endpoint, cfg.S3Region, cfg.S3Bucket, cfg.S3Access, cfg.S3Secret, cfg.S3PathStyle)
		if err != nil {
			return nil, err
		}
		if err := s3.Check(ctx); err != nil {
			return nil, err
		}
		st = s3
		slog.Info("хранилище: S3", "endpoint", cfg.S3Endpoint, "bucket", cfg.S3Bucket)
	default:
		return nil, fmt.Errorf("CB_STORAGE должен быть local или s3, получено %q", kind)
	}
	if cfg.EncryptionKey != "" {
		enc, err := storage.NewEncrypted(st, cfg.EncryptionKey)
		if err != nil {
			return nil, err
		}
		return enc, nil
	}
	return st, nil
}

// background — периодические задачи.
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
			f.CleanupUploads(ctx)
			f.NotifyExpiringShares(ctx)
			a.Cleanup(ctx)
		}
	}
}

func runCommand(ctx context.Context, cfg config.Config, database *sql.DB, a *auth.Service, loc *time.Location, args []string) error {
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
	case "backup":
		fs := flag.NewFlagSet("backup", flag.ExitOnError)
		out := fs.String("o", "", "файл архива (.tar.gz), - для stdout")
		fs.Parse(args[1:])
		if *out == "" {
			*out = "cloudbox-" + time.Now().Format("2006-01-02-1504") + ".tar.gz"
		}
		st, err := openStorage(ctx, cfg, cfg.Storage)
		if err != nil {
			return err
		}
		w := os.Stdout
		if *out != "-" {
			if w, err = os.Create(*out); err != nil {
				return err
			}
			defer w.Close()
		}
		m, err := backup.Create(ctx, cfg.DatabaseURL, database, st, w, os.TempDir())
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "готово: %s — %d файлов, %d МБ\n", *out, m.Blobs, m.Bytes>>20)
	case "migrate-storage":
		fs := flag.NewFlagSet("migrate-storage", flag.ExitOnError)
		to := fs.String("to", "", "куда переносить: s3 или local (откуда — текущее CB_STORAGE)")
		fs.Parse(args[1:])
		if (*to != "s3" && *to != "local") || *to == cfg.Storage {
			return errors.New("использование: cloudbox migrate-storage -to s3|local (должно отличаться от CB_STORAGE)")
		}
		src, err := openStorage(ctx, cfg, cfg.Storage)
		if err != nil {
			return err
		}
		dst, err := openStorage(ctx, cfg, *to)
		if err != nil {
			return err
		}
		err = backup.CopyBlobs(ctx, database, src, dst, func(done, total int) {
			if done%100 == 0 || done == total {
				fmt.Fprintf(os.Stderr, "\rперенесено %d из %d", done, total)
			}
		})
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		fmt.Printf("готово. Теперь установите CB_STORAGE=%s и перезапустите CloudBox.\n", *to)
	case "reindex":
		f := &files.Service{DB: database, Loc: loc}
		n, err := f.ReindexAll(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("в очереди на индексацию: %d файлов (обработаются при работающем сервере)\n", n)
	default:
		return fmt.Errorf("неизвестная команда %q (есть: useradd, passwd, backup, restore, migrate-storage, reindex)", args[0])
	}
	return nil
}

func cmdRestore(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	in := fs.String("i", "", "файл архива (.tar.gz)")
	fs.Parse(args)
	if *in == "" {
		return errors.New("использование: cloudbox restore -i АРХИВ.tar.gz (база должна быть пустой)")
	}
	f, err := os.Open(*in)
	if err != nil {
		return err
	}
	defer f.Close()
	database, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer database.Close()
	st, err := openStorage(ctx, cfg, cfg.Storage)
	if err != nil {
		return err
	}
	n, err := backup.Restore(ctx, cfg.DatabaseURL, database, st, f, os.TempDir())
	if err != nil {
		return err
	}
	fmt.Printf("восстановлено: база данных и %d файлов в хранилище %s\n", n, strings.ToUpper(cfg.Storage))
	return nil
}
