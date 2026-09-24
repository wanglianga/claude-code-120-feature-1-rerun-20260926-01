package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
)

const sessTTL = 8 * time.Hour

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if c, err := r.Cookie("token"); err == nil {
			token = c.Value
		} else if h := r.Header.Get("Authorization"); len(h) > 7 && h[:7] == "Bearer " {
			token = h[7:]
		}
		if token == "" {
			writeErr(w, http.StatusUnauthorized, "未登录")
			return
		}
		raw, err := s.rdb.Get(r.Context(), "sess:"+token).Result()
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "会话已失效")
			return
		}
		var u sessionUser
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			writeErr(w, http.StatusUnauthorized, "会话异常")
			return
		}
		ctx := context.WithValue(r.Context(), userCtx, u)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func role(roles ...string) func(http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, r := range roles {
		allowed[r] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u := currentUser(r)
			if !allowed[u.Role] {
				writeErr(w, http.StatusForbidden, "当前角色无此操作权限（"+u.Role+"）")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	var u sessionUser
	var hash string
	err := s.pool.QueryRow(r.Context(),
		`SELECT id,username,full_name,role,password_hash FROM users WHERE username=$1 AND active=true`,
		req.Username).Scan(&u.ID, &u.Username, &u.FullName, &u.Role, &hash)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		writeErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}

	b := make([]byte, 24)
	_, _ = rand.Read(b)
	token := hex.EncodeToString(b)
	data, _ := json.Marshal(u)
	if err := s.rdb.Set(r.Context(), "sess:"+token, data, sessTTL).Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, "会话写入失败")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "token",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessTTL),
	})
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("token"); err == nil {
		s.rdb.Del(r.Context(), "sess:"+c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "token", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, currentUser(r))
}

var _ = chi.URLParam
