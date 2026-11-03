package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/andrk774/cloudbox/internal/auth"
	"github.com/andrk774/cloudbox/internal/media"
	"github.com/andrk774/cloudbox/internal/secure"
)

// ---------- вход ----------

func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	n, err := s.Auth.CountUsers(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"registration_open": (s.Cfg.AllowRegistration && s.Cfg.PasswordLogin) || n == 0,
		"first_user":        n == 0,
		"logged_in":         userFrom(r) != nil,
		"password_login":    s.Cfg.PasswordLogin || n == 0,
		"oidc":              s.OIDC != nil,
		"oidc_name":         s.Cfg.OIDCName,
	})
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if !s.allow(r, "register", 10, time.Hour) {
		writeErr(w, http.StatusTooManyRequests, "слишком много попыток, попробуйте позже")
		return
	}
	n, err := s.Auth.CountUsers(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	if n > 0 && (!s.Cfg.AllowRegistration || !s.Cfg.PasswordLogin) {
		writeErr(w, http.StatusForbidden, "регистрация закрыта — попросите администратора создать учётную запись")
		return
	}
	var c credentials
	if err := readJSON(r, &c); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	u, err := s.Auth.CreateUser(r.Context(), c.Username, c.Password, false, -1)
	if err != nil {
		fail(w, err)
		return
	}
	s.startSession(w, r, u, "password")
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.allow(r, "login", 10, 5*time.Minute) {
		writeErr(w, http.StatusTooManyRequests, "слишком много попыток входа, подождите 5 минут")
		return
	}
	var c credentials
	if err := readJSON(r, &c); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	u, err := s.Auth.Authenticate(r.Context(), c.Username, c.Password)
	if err == nil && !s.Cfg.PasswordLogin && !u.IsAdmin {
		writeErr(w, http.StatusForbidden, "вход по паролю отключён — используйте "+s.Cfg.OIDCName)
		return
	}
	if err != nil {
		s.Auth.LogLogin(r.Context(), 0, c.Username, clientIP(r), r.UserAgent(), false, "password")
		fail(w, err)
		return
	}
	if u.TOTP {
		writeJSON(w, 200, map[string]any{"totp_required": true, "ticket": s.Auth.Ticket(u.ID)})
		return
	}
	s.Auth.LogLogin(r.Context(), u.ID, u.Username, clientIP(r), r.UserAgent(), true, "password")
	s.startSession(w, r, u, "password")
}

// loginTOTP — второй шаг входа: код из приложения или код восстановления.
func (s *Server) loginTOTP(w http.ResponseWriter, r *http.Request) {
	if !s.allow(r, "totp", 10, 5*time.Minute) {
		writeErr(w, http.StatusTooManyRequests, "слишком много попыток, подождите 5 минут")
		return
	}
	var req struct {
		Ticket string `json:"ticket"`
		Code   string `json:"code"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	uid, valid := s.Auth.CheckTicket(req.Ticket)
	if !valid {
		writeErr(w, http.StatusUnauthorized, "время на ввод кода истекло — войдите заново")
		return
	}
	u, err := s.Auth.GetUser(r.Context(), uid)
	if err != nil {
		fail(w, err)
		return
	}
	good, err := s.Auth.CheckSecondFactor(r.Context(), uid, req.Code)
	if err != nil {
		fail(w, err)
		return
	}
	if !good {
		s.Auth.LogLogin(r.Context(), uid, u.Username, clientIP(r), r.UserAgent(), false, "totp")
		fail(w, auth.ErrBadCode)
		return
	}
	s.Auth.LogLogin(r.Context(), uid, u.Username, clientIP(r), r.UserAgent(), true, "password+totp")
	if left := s.Auth.RecoveryCodesLeft(r.Context(), uid); left <= 2 {
		s.Files.Notify(r.Context(), uid, "security", "Заканчиваются коды восстановления",
			"Осталось кодов: "+strconv.Itoa(left)+". Отключите и снова включите 2FA, чтобы получить новые.", "#/settings", "")
	}
	s.startSession(w, r, u, "password+totp")
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u *auth.User, method string) {
	tok, err := s.Auth.CreateSession(r.Context(), u.ID, r.UserAgent(), clientIP(r), method)
	if err != nil {
		fail(w, err)
		return
	}
	s.setSessionCookie(w, tok)
	writeJSON(w, 200, map[string]any{"user": u, "token": tok})
}

func (s *Server) setSessionCookie(w http.ResponseWriter, tok string) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true,
		Secure: strings.HasPrefix(s.Cfg.PublicURL, "https://"), SameSite: http.SameSiteLaxMode,
		MaxAge: int(auth.SessionTTL.Seconds()),
	})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.Auth.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	okJSON(w)
}

// ---------- OIDC ----------

const oidcCookie = "cb_oidc"

func (s *Server) oidcStart(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		http.NotFound(w, r)
		return
	}
	state, nonce, verifier := secure.Token(16), secure.Token(16), secure.Token(32)
	u, err := s.OIDC.AuthURL(r.Context(), state, nonce, verifier)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	val := state + "." + nonce + "." + verifier
	http.SetCookie(w, &http.Cookie{Name: oidcCookie, Value: val + "." + secure.HMAC(s.Cfg.Secret, "oidc|"+val),
		Path: "/api/auth/oidc/", HttpOnly: true, MaxAge: 600, SameSite: http.SameSiteLaxMode,
		Secure: strings.HasPrefix(s.Cfg.PublicURL, "https://")})
	http.Redirect(w, r, u, http.StatusFound)
}

func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		http.NotFound(w, r)
		return
	}
	failPage := func(msg string) {
		http.Redirect(w, r, "/#/login?error="+strings.ReplaceAll(msg, " ", "+"), http.StatusFound)
	}
	c, err := r.Cookie(oidcCookie)
	if err != nil {
		failPage("сессия входа истекла, попробуйте ещё раз")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oidcCookie, Value: "", Path: "/api/auth/oidc/", MaxAge: -1})
	parts := strings.Split(c.Value, ".")
	if len(parts) != 4 || !secure.Equal(parts[3], secure.HMAC(s.Cfg.Secret, "oidc|"+strings.Join(parts[:3], "."))) {
		failPage("неверное состояние входа")
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		failPage("провайдер отклонил вход: " + e)
		return
	}
	if !secure.Equal(q.Get("state"), parts[0]) {
		failPage("неверное состояние входа")
		return
	}
	claims, err := s.OIDC.Exchange(r.Context(), q.Get("code"), parts[2], parts[1])
	if err != nil {
		s.Auth.LogLogin(r.Context(), 0, "", clientIP(r), r.UserAgent(), false, "oidc")
		failPage(err.Error())
		return
	}
	name := claims.PreferredUsername
	if name == "" {
		name = claims.Name
	}
	u, err := s.Auth.OIDCUser(r.Context(), claims.Sub, name, claims.Email, s.Cfg.OIDCAutoCreate, s.Cfg.OIDCLinkByName)
	if err != nil {
		failPage(err.Error())
		return
	}
	s.Auth.LogLogin(r.Context(), u.ID, u.Username, clientIP(r), r.UserAgent(), true, "oidc")
	tok, err := s.Auth.CreateSession(r.Context(), u.ID, r.UserAgent(), clientIP(r), "oidc")
	if err != nil {
		failPage("не удалось создать сессию")
		return
	}
	s.setSessionCookie(w, tok)
	http.Redirect(w, r, "/", http.StatusFound)
}

// ---------- профиль ----------

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	used, quota, err := s.Files.Usage(r.Context(), u.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"user": u, "used_bytes": used, "quota_bytes": quota,
		"webdav_url":       s.Cfg.PublicURL + "/dav/",
		"max_upload_bytes": s.Cfg.MaxUploadMB << 20,
		"chunk_size":       8 << 20,
		"unread":           s.Files.UnreadCount(r.Context(), u.ID),
		"recovery_left":    s.Auth.RecoveryCodesLeft(r.Context(), u.ID),
		"office_exts":      s.WOPI.Extensions(r.Context()),
		"thumbs": map[string]bool{
			"video": media.HasFFmpeg(),
			"pdf":   media.HasPdftoppm(),
		},
		"encryption": s.Cfg.EncryptionKey != "",
	})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var req struct {
		Old string `json:"old_password"`
		New string `json:"new_password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if _, err := s.Auth.Authenticate(r.Context(), u.Username, req.Old); err != nil && !u.OIDC {
		writeErr(w, 400, "текущий пароль указан неверно")
		return
	}
	if err := s.Auth.SetPassword(r.Context(), u.ID, req.New); err != nil {
		fail(w, err)
		return
	}
	s.startSession(w, r, u, "password") // старые сессии сброшены — выдаём новую
}

func (s *Server) listAppPasswords(w http.ResponseWriter, r *http.Request) {
	list, err := s.Auth.ListAppPasswords(r.Context(), userFrom(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) createAppPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	plain, ap, err := s.Auth.CreateAppPassword(r.Context(), userFrom(r).ID, req.Name)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"password": plain, "app_password": ap})
}

func (s *Server) deleteAppPassword(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Auth.DeleteAppPassword(r.Context(), userFrom(r).ID, id); err != nil {
		fail(w, err)
		return
	}
	okJSON(w)
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	list, err := s.Auth.ListSessions(r.Context(), userFrom(r).ID, tokenFrom(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Auth.DeleteSessionByID(r.Context(), userFrom(r).ID, id); err != nil {
		fail(w, err)
		return
	}
	okJSON(w)
}

func (s *Server) deleteOtherSessions(w http.ResponseWriter, r *http.Request) {
	if err := s.Auth.DeleteOtherSessions(r.Context(), userFrom(r).ID, tokenFrom(r)); err != nil {
		fail(w, err)
		return
	}
	okJSON(w)
}

func (s *Server) listLogins(w http.ResponseWriter, r *http.Request) {
	list, err := s.Auth.ListLogins(r.Context(), userFrom(r).ID, 50)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) totpSetup(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if u.TOTP {
		writeErr(w, 400, "двухфакторная аутентификация уже включена")
		return
	}
	sec, err := s.Auth.SetupTOTP(r.Context(), u.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"secret": sec, "uri": secure.TOTPURI(sec, "CloudBox", u.Username)})
}

func (s *Server) totpEnable(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	codes, err := s.Auth.EnableTOTP(r.Context(), userFrom(r).ID, req.Code)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"recovery_codes": codes})
}

func (s *Server) totpDisable(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var req struct {
		Code string `json:"code"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	good, err := s.Auth.CheckSecondFactor(r.Context(), u.ID, req.Code)
	if err != nil {
		fail(w, err)
		return
	}
	if !good {
		fail(w, auth.ErrBadCode)
		return
	}
	if err := s.Auth.DisableTOTP(r.Context(), u.ID); err != nil {
		fail(w, err)
		return
	}
	okJSON(w)
}

// ---------- администрирование ----------

func (s *Server) adminListUsers(w http.ResponseWriter, r *http.Request) {
	list, err := s.Auth.ListUsers(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) adminCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		IsAdmin    bool   `json:"is_admin"`
		QuotaBytes *int64 `json:"quota_bytes"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	quota := int64(-1)
	if req.QuotaBytes != nil {
		quota = *req.QuotaBytes
	}
	u, err := s.Auth.CreateUser(r.Context(), req.Username, req.Password, req.IsAdmin, quota)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, u)
}

func (s *Server) adminUpdateUser(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if _, err := s.Auth.GetUser(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	var req struct {
		QuotaBytes *int64  `json:"quota_bytes"`
		IsAdmin    *bool   `json:"is_admin"`
		Password   *string `json:"password"`
		Reset2FA   bool    `json:"reset_2fa"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.IsAdmin != nil && !*req.IsAdmin && id == userFrom(r).ID {
		writeErr(w, 400, "нельзя снять права администратора с самого себя")
		return
	}
	if req.Password != nil {
		if err := s.Auth.SetPassword(r.Context(), id, *req.Password); err != nil {
			fail(w, err)
			return
		}
	}
	if req.Reset2FA {
		if err := s.Auth.DisableTOTP(r.Context(), id); err != nil {
			fail(w, err)
			return
		}
	}
	if err := s.Auth.UpdateUser(r.Context(), id, req.QuotaBytes, req.IsAdmin); err != nil {
		fail(w, err)
		return
	}
	u, _ := s.Auth.GetUser(r.Context(), id)
	writeJSON(w, 200, u)
}

func (s *Server) adminDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if id == userFrom(r).ID {
		writeErr(w, 400, "нельзя удалить самого себя")
		return
	}
	if err := s.Auth.DeleteUser(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	go s.Files.GC(context.Background())
	okJSON(w)
}

func (s *Server) adminListGroups(w http.ResponseWriter, r *http.Request) {
	list, err := s.Files.ListGroups(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) adminCreateGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	g, err := s.Files.CreateGroup(r.Context(), req.Name)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, g)
}

func (s *Server) adminDeleteGroup(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Files.DeleteGroup(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	okJSON(w)
}

func (s *Server) adminSetMembers(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var req struct {
		UserIDs []int64 `json:"user_ids"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.Files.SetGroupMembers(r.Context(), id, req.UserIDs); err != nil {
		fail(w, err)
		return
	}
	okJSON(w)
}
