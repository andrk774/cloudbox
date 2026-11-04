// cloudbox-sync — двусторонняя синхронизация локальной папки с CloudBox (Windows, Linux, macOS).
//
//	cloudbox-sync -server https://cloud.example.ru -user andrey -local ~/CloudBox -remote /Документы -watch 30s
//
// Пароль — пароль приложения из настроек CloudBox: флаг -password или переменная CLOUDBOX_PASSWORD.
// Правила синхронизации — в internal/syncclient. Графическая версия — cmd/cloudbox-desktop.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/andrk774/cloudbox/internal/syncclient"
)

func main() {
	server := flag.String("server", os.Getenv("CLOUDBOX_SERVER"), "адрес CloudBox, например https://cloud.example.ru")
	user := flag.String("user", os.Getenv("CLOUDBOX_USER"), "имя пользователя")
	pass := flag.String("password", "", "пароль приложения (лучше через CLOUDBOX_PASSWORD)")
	local := flag.String("local", "", "локальная папка")
	remote := flag.String("remote", "/", "папка на сервере, например /Документы")
	watch := flag.Duration("watch", 0, "повторять синхронизацию с этим интервалом (например 30s); 0 — один раз")
	dry := flag.Bool("dry-run", false, "только показать, что будет сделано")
	flag.Parse()
	if *pass == "" {
		*pass = os.Getenv("CLOUDBOX_PASSWORD")
	}
	if *server == "" || *user == "" || *pass == "" || *local == "" {
		flag.Usage()
		os.Exit(2)
	}
	s, err := syncclient.New(syncclient.Config{Server: *server, User: *user, Password: *pass, Local: *local, Remote: *remote, DryRun: *dry}, log.Printf)
	if err != nil {
		log.Fatal(err)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	for {
		start := time.Now()
		if st, err := s.Run(); err != nil {
			log.Printf("ошибка синхронизации: %v", err)
		} else if st.Changed() {
			log.Printf("готово за %s: загружено %d, скачано %d, удалено локально %d, на сервере %d, конфликтов %d",
				time.Since(start).Round(time.Millisecond), st.Up, st.Down, st.DelLocal, st.DelRemote, st.Conflicts)
		}
		if *watch == 0 {
			return
		}
		select {
		case <-stop:
			return
		case <-time.After(*watch):
		}
	}
}
