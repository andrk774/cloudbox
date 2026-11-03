package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrk774/cloudbox/internal/syncclient"
)

func testApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	a := newApp(filepath.Join(dir, "desktop.json"), &Config{Remote: "/"})
	a.addr = "127.0.0.1:47000"
	checkServer = func(server, user, pw string) error {
		if pw != "good" {
			return errors.New("неверное имя пользователя или пароль приложения")
		}
		return nil
	}
	t.Cleanup(func() { checkServer = syncclient.Check })
	return a
}

func do(h http.Handler, method, target, host, origin, cookie string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(`{}`))
	r.Host = host
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestGuard(t *testing.T) {
	a := testApp(t)
	h := a.Handler()
	host := a.addr

	if w := do(h, "GET", "/api/status", host, "", ""); w.Code != 403 {
		t.Fatalf("без токена: %d", w.Code)
	}
	if w := do(h, "GET", "/api/status", host, "", "wrong"); w.Code != 403 {
		t.Fatalf("чужой токен: %d", w.Code)
	}
	if w := do(h, "GET", "/?t=wrong", host, "", ""); w.Code != 403 {
		t.Fatalf("неверный токен в ссылке: %d", w.Code)
	}
	w := do(h, "GET", "/?t="+a.token, host, "", "")
	if w.Code != 302 || !strings.Contains(w.Header().Get("Set-Cookie"), "SameSite=Strict") || !strings.Contains(w.Header().Get("Set-Cookie"), "HttpOnly") {
		t.Fatalf("вход по ссылке: %d %q", w.Code, w.Header().Get("Set-Cookie"))
	}
	if w := do(h, "GET", "/api/status", host, "", a.token); w.Code != 200 {
		t.Fatalf("с токеном: %d", w.Code)
	}
	// DNS rebinding: чужой домен, указывающий на 127.0.0.1, не пускаем даже с правильным cookie
	if w := do(h, "GET", "/api/status", "evil.example:47000", "", a.token); w.Code != 403 {
		t.Fatalf("чужой Host: %d", w.Code)
	}
	// CSRF: изменяющий запрос с чужого сайта
	if w := do(h, "POST", "/api/sync", host, "https://evil.example", a.token); w.Code != 403 {
		t.Fatalf("чужой Origin: %d", w.Code)
	}
	if w := do(h, "POST", "/api/sync", host, "", a.token); w.Code != 403 {
		t.Fatalf("POST без Origin: %d", w.Code)
	}
	if w := do(h, "POST", "/api/sync", host, "http://"+host, a.token); w.Code != 200 {
		t.Fatalf("свой Origin: %d", w.Code)
	}
	if w := do(h, "GET", "/", "localhost:47000", "", a.token); w.Code != 200 || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("localhost: %d", w.Code)
	}
}

func TestConfigRoundtrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "desktop.json")
	c := &Config{Server: "https://cloud.example.ru", User: "ana", Password: "abcde-fghij", Local: "/x", Remote: "/Документы", Interval: 30}
	if err := saveConfig(p, c); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if strings.Contains(string(raw), `"password"`) || (strings.Contains(string(raw), "abcde-fghij")) {
		t.Fatalf("пароль открытым текстом в файле настроек:\n%s", raw)
	}
	got, err := loadConfig(p)
	if err != nil || got.Password != "abcde-fghij" || got.Remote != "/Документы" || got.Interval != 30 || !got.Ready() {
		t.Fatalf("roundtrip: %+v %v", got, err)
	}
	empty, err := loadConfig(filepath.Join(t.TempDir(), "none.json"))
	if err != nil || empty.Ready() || empty.Remote != "/" {
		t.Fatalf("нет файла: %+v %v", empty, err)
	}
}

func TestConfigureAndSync(t *testing.T) {
	a := testApp(t)
	local := t.TempDir()
	if a.Status().State != "setup" {
		t.Fatal("без настроек — состояние setup")
	}
	in := ConfigInput{Server: "https://cloud.example.ru/", User: "ana", Password: "bad", Local: local, Remote: "Документы"}
	if err := a.Configure(in); err == nil {
		t.Fatal("неверный пароль должен отклоняться")
	}
	if err := a.Configure(ConfigInput{Server: "ftp://x", User: "ana", Password: "good", Local: local}); err == nil {
		t.Fatal("неверный адрес сервера")
	}
	in.Password = "good"
	if err := a.Configure(in); err != nil {
		t.Fatal(err)
	}
	st := a.Status()
	if st.State != "idle" || st.Server != "https://cloud.example.ru" || st.Remote != "/Документы" || !st.HasPassword {
		t.Fatalf("после настройки: %+v", st)
	}
	// пароль не меняли — пустое поле оставляет сохранённый
	in.Password = ""
	in.Interval = 30
	if err := a.Configure(in); err != nil {
		t.Fatalf("повторное сохранение без пароля: %v", err)
	}

	// смена папки на сервере для той же локальной папки сбрасывает состояние движка
	os.WriteFile(filepath.Join(local, stateFile), []byte(`{}`), 0o644)
	in.Remote = "/Другое"
	if err := a.Configure(in); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(local, stateFile)); !os.IsNotExist(err) {
		t.Fatal("состояние старой пары папок должно быть сброшено")
	}

	// проход синхронизации: успех и ошибка
	a.runSyncer = func(*syncclient.Syncer) (syncclient.Stats, error) { return syncclient.Stats{Up: 2}, nil }
	a.syncOnce()
	if st := a.Status(); st.State != "idle" || st.Stats.Up != 2 || st.LastOK == nil || len(st.Logs) == 0 {
		t.Fatalf("успешный проход: %+v", st)
	}
	a.runSyncer = func(*syncclient.Syncer) (syncclient.Stats, error) { return syncclient.Stats{}, errors.New("сервер недоступен") }
	a.syncOnce()
	if st := a.Status(); st.State != "error" || st.Error != "сервер недоступен" {
		t.Fatalf("ошибка: %+v", st)
	}
	if err := a.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	called := false
	a.runSyncer = func(*syncclient.Syncer) (syncclient.Stats, error) { called = true; return syncclient.Stats{}, nil }
	a.syncOnce()
	if called || a.Status().State != "paused" {
		t.Fatal("на паузе синхронизация не запускается")
	}
}

func TestIcon(t *testing.T) {
	for _, s := range []string{"idle", "syncing", "paused", "error", "setup"} {
		if b := iconFor(s); len(b) < 100 {
			t.Fatalf("%s: значок пустой", s)
		}
	}
}
