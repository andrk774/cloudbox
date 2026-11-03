// mailtest — тестовый IMAP/SMTP-сервер в памяти для сквозных тестов почты (scripts/e2e_mail.py).
// Не для продакшена: без шифрования, один пользователь username/password.
//
//	go run ./internal/mail/mailtest/cmd/mailtest -imap 127.0.0.1:1143 -smtp 127.0.0.1:1025
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/andrk774/cloudbox/internal/mail/mailtest"
)

func main() {
	imapAddr := flag.String("imap", "127.0.0.1:1143", "адрес IMAP")
	smtpAddr := flag.String("smtp", "127.0.0.1:1025", "адрес SMTP")
	flag.Parse()
	s, err := mailtest.Start(*imapAddr, *smtpAddr)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("IMAP %s, SMTP %s, пользователь %s / %s\n", s.IMAPAddr, s.SMTPAddr, mailtest.User, mailtest.Password)
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	<-c
	s.Close()
}
